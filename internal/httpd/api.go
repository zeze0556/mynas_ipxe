package httpd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ipxe-server/internal/db"
	"ipxe-server/internal/iso"
)

func (s *Server) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/users", s.handleUsers)
	mux.HandleFunc("/api/hosts", s.handleHosts)
	mux.HandleFunc("/api/templates", s.handleTemplates)
	mux.HandleFunc("/api/isos", s.handleISOs)
	mux.HandleFunc("/api/isos/import", s.handleImportISO)
	mux.HandleFunc("/api/isos/refresh", s.handleRefreshISO)
	mux.HandleFunc("/api/injections", s.handleInjections)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var list []db.User
		s.db.Find(&list)
		writeJSON(w, 200, list)
	case http.MethodPost:
		var u db.User
		if err := readJSON(r, &u); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if u.Name == "" {
			writeJSON(w, 400, map[string]string{"error": "name required"})
			return
		}
		if u.Token == "" {
			u.Token = randToken(16)
		}
		if err := s.db.Create(&u).Error; err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, u)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, 400, map[string]string{"error": "id required"})
			return
		}
		s.db.Delete(&db.User{}, id)
		writeJSON(w, 200, map[string]string{"ok": "1"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var list []db.Host
		s.db.Find(&list)
		writeJSON(w, 200, list)
	case http.MethodPost:
		var h db.Host
		if err := readJSON(r, &h); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		h.MAC = normMac(h.MAC)
		if h.MAC == "" || h.UserID == 0 {
			writeJSON(w, 400, map[string]string{"error": "mac and user_id required"})
			return
		}
		// 用户必须存在
		var u db.User
		if s.db.First(&u, h.UserID).Error != nil {
			writeJSON(w, 400, map[string]string{"error": "user not found"})
			return
		}
		if err := s.db.Save(&h).Error; err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, h)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, 400, map[string]string{"error": "id required"})
			return
		}
		s.db.Delete(&db.Host{}, id)
		writeJSON(w, 200, map[string]string{"ok": "1"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var list []db.Template
		s.db.Find(&list)
		writeJSON(w, 200, list)
	case http.MethodPost:
		var t db.Template
		if err := readJSON(r, &t); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if t.Name == "" || t.Kind == "" || t.Scope == "" {
			writeJSON(w, 400, map[string]string{"error": "name/kind/scope required"})
			return
		}
		if err := s.db.Create(&t).Error; err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, t)
	case http.MethodPut:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, 400, map[string]string{"error": "id required"})
			return
		}
		var existing db.Template
		if s.db.First(&existing, id).Error != nil {
			writeJSON(w, 404, map[string]string{"error": "template not found"})
			return
		}
		var patch db.Template
		if err := readJSON(r, &patch); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if patch.Name == "" || patch.Kind == "" || patch.Scope == "" {
			writeJSON(w, 400, map[string]string{"error": "name/kind/scope required"})
			return
		}
		// 用 map 显式更新：struct 更新会跳过零值，导致无法清空 body 或解绑 ISO。
		fields := map[string]any{
			"name": patch.Name, "scope": patch.Scope, "owner_id": patch.OwnerID,
			"kind": patch.Kind, "iso_id": patch.ISOID, "body": patch.Body,
			"iscsi_target": patch.ISCSITarget, "initiator_iqn": patch.InitiatorIQN, "nfs_root": patch.NFSRoot,
			"nfs_upper": patch.NFSUpper,
		}
		if err := s.db.Model(&existing).Updates(fields).Error; err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		s.db.First(&existing, id)
		writeJSON(w, 200, existing)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, 400, map[string]string{"error": "id required"})
			return
		}
		s.db.Delete(&db.Template{}, id)
		writeJSON(w, 200, map[string]string{"ok": "1"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleISOs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var list []db.ISO
		s.db.Order("id").Find(&list)
		writeJSON(w, 200, list)
	case http.MethodDelete:
		s.deleteISO(w, r)
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// deleteISO 删除镜像记录。
//   - 被模板/注入引用时返回 409 并列出引用，避免静默留下悬空 iso_id。
//   - force=1 先解绑模板并删除注入记录，再删镜像。
//   - purge=1 额外删除磁盘镜像与解包缓存；仍有其它同名记录时跳过，避免误删共享文件。
//   - 默认只删数据库记录，不动磁盘。
func (s *Server) deleteISO(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	var rec db.ISO
	if s.db.First(&rec, id).Error != nil {
		writeJSON(w, 404, map[string]string{"error": "iso not found"})
		return
	}

	var tmpls []db.Template
	s.db.Select("id", "name", "scope").Where("iso_id = ?", rec.ID).Find(&tmpls)
	var injs []db.Injection
	s.db.Select("id", "iso_id", "scope", "image_path").Where("iso_id = ?", rec.ID).Find(&injs)

	force := r.URL.Query().Get("force") == "1"
	if (len(tmpls) > 0 || len(injs) > 0) && !force {
		writeJSON(w, 409, map[string]any{
			"error":      "iso is still referenced; unbind templates first or use force=1",
			"templates":  tmpls,
			"injections": injs,
		})
		return
	}

	unbound, removed := 0, 0
	if len(tmpls) > 0 {
		res := s.db.Model(&db.Template{}).Where("iso_id = ?", rec.ID).Update("iso_id", nil)
		if res.Error != nil {
			writeJSON(w, 500, map[string]string{"error": res.Error.Error()})
			return
		}
		unbound = int(res.RowsAffected)
	}
	if len(injs) > 0 {
		res := s.db.Where("iso_id = ?", rec.ID).Delete(&db.Injection{})
		if res.Error != nil {
			writeJSON(w, 500, map[string]string{"error": res.Error.Error()})
			return
		}
		removed = int(res.RowsAffected)
	}
	if err := s.db.Delete(&db.ISO{}, rec.ID).Error; err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	purged := false
	if r.URL.Query().Get("purge") == "1" {
		var same int64
		s.db.Model(&db.ISO{}).Where("filename = ? AND id <> ?", rec.Filename, rec.ID).Count(&same)
		if same == 0 {
			_ = os.Remove(filepath.Join(s.cfg.IsosDir(), rec.Filename))
			cacheRoot := s.cfg.CacheDir() + string(os.PathSeparator)
			if rec.ExtractedPath != "" && strings.HasPrefix(rec.ExtractedPath, cacheRoot) {
				_ = os.RemoveAll(rec.ExtractedPath)
			}
			purged = true
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok":                 1,
		"deleted":            map[string]any{"id": rec.ID, "name": rec.Name, "filename": rec.Filename},
		"unbound_templates":  unbound,
		"deleted_injections": removed,
		"purged":             purged,
	})
}

// handleRefreshISO 从 data/isos/<filename> 重新读取指定记录并重新解包。
// 用于 ISO 文件被外部替换后无需再填路径：记录 ID 与模板 iso_id 绑定保持不变。
func (s *Server) handleRefreshISO(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	var rec db.ISO
	if s.db.First(&rec, id).Error != nil {
		writeJSON(w, 404, map[string]string{"error": "iso not found"})
		return
	}
	srcPath := filepath.Join(s.cfg.IsosDir(), rec.Filename)
	f, err := openLocalImage(srcPath)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	defer f.Close()
	s.importImage(w, f, rec.Filename, srcPath, rec.ID)
}

// handleImportISO 接收上传的镜像（ISO/IMG/VHD/WIM），登记记录。
//   - ISO：解包到 cache，供 /c/ overlay 内容服务（linux/windows 模板）。
//   - IMG/VHD/WIM：裸盘/映像类，不解包，原样落盘到 isos 目录，供 /r/ 原生启动。
//     · IMG：raw 模板 sanboot 为虚拟硬盘。
//     · VHD：固定格式自动剥离 512B footer 转 raw 后走 sanboot；动态/VHDX 原样落盘（需自行转换）。
//     · WIM：wim 模板经 wimboot 启动（WinPE / 网络安装 Windows）。
func (s *Server) handleImportISO(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	// 来源一：浏览器 multipart 上传（iso 字段）；来源二：服务器本地路径 JSON {"path": "..."}。
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(4 << 30); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		f, hdr, err := r.FormFile("iso")
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "iso field required"})
			return
		}
		defer f.Close()
		s.importImage(w, f, filepath.Base(hdr.Filename), "", 0)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "path required: " + err.Error()})
		return
	}
	src, err := openLocalImage(req.Path)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	defer src.Close()
	s.importImage(w, src, filepath.Base(src.Name()), src.Name(), 0)
}

// openLocalImage 校验服务器本地镜像路径。
// 约束：绝对路径、非符号链接的普通文件、扩展名在镜像白名单内，避免被用作任意文件读取通道。
func openLocalImage(p string) (*os.File, error) {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		return nil, fmt.Errorf("path must be an absolute path")
	}
	clean := filepath.Clean(p)
	switch strings.ToLower(filepath.Ext(clean)) {
	case ".iso", ".img", ".ima", ".vhd", ".vhdx", ".wim":
	default:
		return nil, fmt.Errorf("unsupported image extension: %s", filepath.Ext(clean))
	}
	fi, err := os.Lstat(clean)
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	f, err := os.Open(clean)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	return f, nil
}

// importImage 把镜像落到 isos 目录并登记。
// srcPath 非空且已位于 isos 目录时原地复用（只算 checksum），否则复制一份，
// 保证 /r/、/c/ 与上传导入的数据路径完全一致。
//
// 幂等语义（按 filename 判身份、按 checksum 判版本）：
//   - 同名文件再次导入 → 更新已有记录（ID 不变，模板 iso_id 绑定自动指向新内容）。
//   - 内容未变 → 跳过解包；内容变化 → 解包到临时目录后原子替换缓存。
//   - 历史遗留的同名重复记录取最小 id 更新，其余保留，由删除 API 清理。
//
// targetID 非 0 时只更新该记录（refresh 语义）；为 0 时按 filename 幂等（import 语义）。
func (s *Server) importImage(w http.ResponseWriter, src io.Reader, filename, srcPath string, targetID uint) {
	// 同名镜像的导入串行化：避免并发请求同时改 data/isos 与 cache 目录。
	isoImportMu.Lock()
	defer isoImportMu.Unlock()

	if filename == "" || filename == "." {
		writeJSON(w, 400, map[string]string{"error": "invalid filename"})
		return
	}
	dst := filepath.Join(s.cfg.IsosDir(), filename)

	reuse := false
	if srcPath != "" {
		absSrc, e1 := filepath.Abs(srcPath)
		absDst, e2 := filepath.Abs(dst)
		reuse = e1 == nil && e2 == nil && absSrc == absDst
	}

	var size int64
	var checksum string
	if reuse {
		f, err := os.Open(srcPath)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		h := sha256.New()
		size, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		checksum = hex.EncodeToString(h.Sum(nil))
	} else {
		out, err := os.Create(dst)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		h := sha256.New()
		size, err = io.Copy(io.MultiWriter(out, h), src)
		out.Close()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		checksum = hex.EncodeToString(h.Sum(nil))
	}

	isoName := strings.TrimSuffix(filename, filepath.Ext(filename))
	format := detectFormat(filename)

	// 身份 = filename（data/isos/<filename> 与 cache/<name> 都是唯一路径），
	// 版本 = checksum。取最小 id，保住既有模板 iso_id 绑定。
	var existing db.ISO
	hasExisting := false
	if targetID != 0 {
		// refresh：点哪一行就更新哪一行，不与同名记录合并。
		hasExisting = s.db.First(&existing, targetID).Error == nil
	} else {
		// import：按 filename 取最小 id；用 Find+Limit 而非 First，避免首次导入刷 WARN 日志。
		var found []db.ISO
		s.db.Where("filename = ?", filename).Order("id").Limit(1).Find(&found)
		if len(found) > 0 {
			existing, hasExisting = found[0], true
		}
	}
	unchanged := hasExisting && existing.Checksum == checksum

	// 存量脏数据提示：同名的其它记录不会自动删除，交给 DELETE API 清理。
	var dupIDs []uint
	if hasExisting {
		s.db.Model(&db.ISO{}).Where("filename = ? AND id <> ?", filename, existing.ID).Pluck("id", &dupIDs)
	}

	rec := db.ISO{
		Name:     isoName,
		Filename: filename,
		Size:     size,
		Checksum: checksum,
		Format:   string(format),
	}

	extracted := 0
	note := ""
	switch format {
	case db.ISOFormatISO:
		cacheDir := filepath.Join(s.cfg.CacheDir(), isoName)
		// 内容未变且缓存仍在 → 跳过 1GB 级解包；否则解到临时目录后原子替换，
		// 防止旧 ISO 独有的文件残留在缓存里、也防止解包失败破坏正在服务的内容。
		if unchanged && existing.ExtractedPath == cacheDir && dirExists(cacheDir) {
			rec.ExtractedPath = cacheDir
		} else {
			n, e := refreshCache(dst, cacheDir)
			if e != nil {
				writeJSON(w, 500, map[string]string{"error": "extract failed: " + e.Error()})
				return
			}
			extracted = n
			rec.ExtractedPath = cacheDir
		}
	case db.ISOFormatVHD:
		stripped, vhdNote, e := stripVHDFooter(dst)
		if e != nil {
			writeJSON(w, 500, map[string]string{"error": "vhd convert: " + e.Error()})
			return
		}
		if stripped {
			rec.Size = size - 512
		}
		if vhdNote != "" {
			note = vhdNote
		}
	}

	status, action := 201, "created"
	if hasExisting {
		status = 200
		if unchanged {
			action = "unchanged"
		} else {
			action = "updated"
		}
		fields := map[string]any{
			"name": isoName, "filename": filename, "size": rec.Size,
			"checksum": checksum, "format": string(format),
			"extracted_path": rec.ExtractedPath,
		}
		if err := s.db.Model(&db.ISO{}).Where("id = ?", existing.ID).Updates(fields).Error; err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		rec.ID = existing.ID
		rec.CreatedAt = existing.CreatedAt
	} else {
		if err := s.db.Create(&rec).Error; err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}

	resp := map[string]any{
		"iso": rec, "format": format, "extracted_files": extracted,
		"action": action, "unchanged": unchanged, "duplicate_ids": dupIDs,
	}
	if note != "" {
		resp["note"] = note
	}
	writeJSON(w, status, resp)
}

// isoImportMu 串行化镜像导入（跨 handler 共享）。
var isoImportMu sync.Mutex

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// refreshCache 把 ISO 解包到临时目录，成功后原子替换 cacheDir。
// ① 新 ISO 删掉的文件不会残留在旧缓存里；② 解包失败不影响正在服务的旧内容。
func refreshCache(isoPath, cacheDir string) (int, error) {
	parent := filepath.Dir(cacheDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(cacheDir)+"-new-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)
	n, err := iso.Import(isoPath, tmp)
	if err != nil {
		return 0, err
	}
	if err := swapCacheDir(tmp, cacheDir); err != nil {
		return 0, err
	}
	return n, nil
}

// swapCacheDir 用 tmpDir 替换 cacheDir；失败时回滚旧目录。
func swapCacheDir(tmpDir, cacheDir string) error {
	backup := cacheDir + ".old-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	hadOld := false
	if _, err := os.Stat(cacheDir); err == nil {
		if err := os.Rename(cacheDir, backup); err != nil {
			return err
		}
		hadOld = true
	}
	if err := os.Rename(tmpDir, cacheDir); err != nil {
		if hadOld {
			_ = os.Rename(backup, cacheDir)
		}
		return err
	}
	if hadOld {
		_ = os.RemoveAll(backup)
	}
	return nil
}

// detectFormat 按扩展名判定镜像格式；未知扩展名按裸盘 IMG 处理（不解包）。
func detectFormat(filename string) db.ISOFormat {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".iso":
		return db.ISOFormatISO
	case ".img", ".ima":
		return db.ISOFormatIMG
	case ".vhd", ".vhdx":
		return db.ISOFormatVHD
	case ".wim":
		return db.ISOFormatWIM
	default:
		return db.ISOFormatIMG
	}
}

// stripVHDFooter 处理 VHD 文件末尾 512B footer：
//   - 固定(fixed) VHD：footer cookie 为 "conectix"，且仅当 (文件大小-512)==footer.CurrentSize
//     时确认是连续 raw 数据，剥离 footer 转 raw，便于 raw 模板经 /r/ 的 sanboot 启动为虚拟硬盘。
//   - 动态(dynamic) VHD：末尾同样带一份 footer 副本，但文件含 header+BAT+数据块，并非纯 raw，
//     不剥离，原样保留并返回提示，由调用方建议用户用 qemu-img 转换。
//   - 非 VHD（如 VHDX，使用不同的 64KB 容器格式）无 "conectix" footer，原样保留。
//
// 返回：stripped 是否已剥离、note 提示文本、err 错误。
func stripVHDFooter(path string) (stripped bool, note string, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, "", err
	}
	sz := fi.Size()
	if sz < 512 {
		return false, "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, "", err
	}
	defer f.Close()
	footer := make([]byte, 512)
	if _, err := f.ReadAt(footer, sz-512); err != nil {
		return false, "", err
	}
	// footer 偏移 0 的 8 字节 cookie 必须是 "conectix"。
	if string(footer[0:8]) != "conectix" {
		return false, "", nil // 非 VHD（如 VHDX），原样保留
	}
	// footer 偏移 48 的 8 字节 CurrentSize（大端）= 虚拟磁盘大小。
	currentSize := binary.BigEndian.Uint64(footer[48:56])
	// 固定 VHD：数据部分大小 == CurrentSize，文件 == 数据 + 512 footer。
	if uint64(sz)-512 == currentSize {
		if err := os.Truncate(path, sz-512); err != nil {
			return false, "", err
		}
		return true, "", nil
	}
	// 动态/差分 VHD：文件非连续 raw，不可无损剥离。
	return false, "动态 VHD 已原样保留，无法无损转为 raw；请用 qemu-img convert -f vpc -O raw 转换后再上传", nil
}

// handleInjections 上传注入文件并登记；按 scope 落到 overlay 目录。
func (s *Server) handleInjections(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var list []db.Injection
		q := r.URL.Query().Get("iso_id")
		if q != "" {
			s.db.Where("iso_id = ?", q).Find(&list)
		} else {
			s.db.Find(&list)
		}
		writeJSON(w, 200, list)
	case http.MethodPost:
		if err := r.ParseMultipartForm(1 << 30); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		isoID, _ := strconv.ParseUint(r.FormValue("iso_id"), 10, 64)
		scope := r.FormValue("scope")
		imagePath := r.FormValue("image_path")
		if isoID == 0 || scope == "" || imagePath == "" {
			writeJSON(w, 400, map[string]string{"error": "iso_id/scope/image_path required"})
			return
		}
		var isoRec db.ISO
		if s.db.First(&isoRec, isoID).Error != nil {
			writeJSON(w, 400, map[string]string{"error": "iso not found"})
			return
		}
		uf, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "file required"})
			return
		}
		defer uf.Close()

		var ownerID *uint
		if o := r.FormValue("owner_id"); o != "" {
			if v, e := strconv.ParseUint(o, 10, 64); e == nil {
				u := uint(v)
				ownerID = &u
			}
		}
		stored := iso.OverlayStoredPath(s.cfg.OverlayDir(), scope, ownerID, isoRec.Name, imagePath)
		if err := os.MkdirAll(filepath.Dir(stored), 0o755); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		out, err := os.Create(stored)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if _, err := io.Copy(out, uf); err != nil {
			out.Close()
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		out.Close()

		rec := db.Injection{
			ISOID:      uint(isoID),
			Scope:      db.InjectionScope(scope),
			OwnerID:    ownerID,
			ImagePath:  imagePath,
			StoredPath: stored,
		}
		if err := s.db.Create(&rec).Error; err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, rec)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, 400, map[string]string{"error": "id required"})
			return
		}
		var rec db.Injection
		if s.db.First(&rec, id).Error == nil {
			_ = os.Remove(rec.StoredPath)
		}
		s.db.Delete(&db.Injection{}, id)
		writeJSON(w, 200, map[string]string{"ok": "1"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}
