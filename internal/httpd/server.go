package httpd

import (
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"text/template"

	"gorm.io/gorm"

	"ipxe-server/config"
	"ipxe-server/internal/db"
	"ipxe-server/internal/iso"
)

// Server 持有配置与数据库，注册所有 HTTP 路由。
type Server struct {
	cfg *config.Config
	db  *gorm.DB
}

// New 构造 HTTP 服务。
func New(cfg *config.Config, db *gorm.DB) *Server { return &Server{cfg: cfg, db: db} }

// Start 启动 HTTP 监听。
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/boot", s.handleBoot)
	mux.HandleFunc("/c/", s.handleContent)
	mux.HandleFunc("/r/", s.handleRaw)
	mux.HandleFunc("/pe/", s.handlePe)
	mux.HandleFunc("/menu", s.handleMenu)
	mux.HandleFunc("/api/boot-preview", s.handleBootPreview)
	s.registerAPI(mux)
	mux.HandleFunc("/", s.handleUI)
	return http.ListenAndServe(s.cfg.HTTPListen, mux)
}

// bootData 注入到 iPXE 模板的变量。
type bootData struct {
	MAC          string
	HostName     string
	UserName     string
	ISOName      string
	ContentURL   string // http://host:port/c/<mac>/<isoname>
	RawURL       string // http://host:port/r/<mac>/<isoname>
	PeURL        string // http://host:port/pe/  PE 支撑文件目录（wimboot/bootmgr.exe/bcd/boot.sdi）
	ISCSITarget  string // iSCSI root-path（外部 target），供 sanhook
	InitiatorIQN string // 客户端 iSCSI initiator IQN，供 set initiator-iqn
	NFSRoot      string // ro 基础 root 导出
	NFSUpper     string // 每客户端 overlay 基础导出
	Vars         map[string]string
}

// handleBoot 按 MAC 选取模板并渲染 iPXE 启动脚本。
func (s *Server) handleBoot(w http.ResponseWriter, r *http.Request) {
	mac := normMac(r.URL.Query().Get("mac"))
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = string(db.KindLinux)
	}
	tmplName := r.URL.Query().Get("template")

	var host *db.Host
	if mac != "" {
		var h db.Host
		if s.db.Where("mac = ?", mac).First(&h).Error == nil {
			host = &h
		}
	}
	// 匹配主机：按数据库逐机开关 boot_to_menu 决定是否进菜单（数据库配置优先）。
	if host != nil && host.BootToMenu {
		writeScript(w, "#!ipxe\nchain /menu?mac="+mac+"\n")
		return
	}
	// DenyUnknownMAC 优先：未知 MAC 在拒绝名单下仍直接拒绝，不进菜单。
	if s.cfg.DenyUnknownMAC && host == nil {
		http.Error(w, "unknown mac denied", http.StatusForbidden)
		return
	}
	// 不匹配主机：全局 menu.enabled 兜底，显示菜单。
	// 条件与 handleMenu 的 fallback（!Enabled || 无分类时退化到 handleBoot）互补，避免 /menu -> /boot -> /menu 循环。
	if host == nil && s.cfg.Menu.Enabled && len(s.cfg.Menu.Categories) > 0 {
		writeScript(w, "#!ipxe\nchain /menu?mac="+mac+"\n")
		return
	}
	// 其余（含 menu.enabled=false 的不匹配主机）：走 pickTemplate 渲染数据库第一个配置。

	tmpl, ok := s.pickTemplate(host, kind, tmplName)
	if !ok {
		writeScript(w, "#!ipxe\necho ERROR: no matching template\nexit 1\n")
		return
	}
	if tmpl.ISOID == nil {
		writeScript(w, "#!ipxe\necho ERROR: template '"+tmpl.Name+"' has no ISO bound\nexit 1\n")
		return
	}
	var isoRec db.ISO
	if s.db.First(&isoRec, *tmpl.ISOID).Error != nil {
		writeScript(w, "#!ipxe\necho ERROR: ISO not found\nexit 1\n")
		return
	}

	hostName, userName := "unknown", "public"
	vars := map[string]string{}
	if host != nil {
		hostName = host.Name
		var u db.User
		if s.db.First(&u, host.UserID).Error == nil {
			userName = u.Name
		}
		vars = host.VarsMap()
	}

	data := bootData{
		MAC:          mac,
		HostName:     hostName,
		UserName:     userName,
		ISOName:      isoRec.Name,
		ContentURL:   fmt.Sprintf("http://%s:%d/c/%s/%s", s.cfg.NextServer, s.cfg.HTTPPort, mac, isoRec.Name),
		RawURL:       fmt.Sprintf("http://%s:%d/r/%s/%s", s.cfg.NextServer, s.cfg.HTTPPort, mac, isoRec.Name),
		PeURL:        fmt.Sprintf("http://%s:%d/pe/", s.cfg.NextServer, s.cfg.HTTPPort),
		ISCSITarget:  tmpl.ISCSITarget,
		InitiatorIQN: tmpl.InitiatorIQN,
		NFSRoot:      tmpl.NFSRoot,
		NFSUpper:     tmpl.NFSUpper,
		Vars:         vars,
	}

	t, err := template.New("boot").Parse(tmpl.Body)
	if err != nil {
		writeScript(w, "#!ipxe\necho ERROR: bad template: "+err.Error()+"\nexit 1\n")
		return
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		writeScript(w, "#!ipxe\necho ERROR: render: "+err.Error()+"\nexit 1\n")
		return
	}
	if strings.TrimSpace(buf.String()) == "" {
		writeScript(w, "#!ipxe\necho ERROR: template '"+tmpl.Name+"' body is empty\nexit 1\n")
		return
	}
	writeScript(w, buf.String())
}

// handleBootPreview 调试用：直接返回渲染后的脚本文本。
func (s *Server) handleBootPreview(w http.ResponseWriter, r *http.Request) {
	s.handleBoot(w, r)
}

// pickTemplate 按 host > user > public 优先级选取模板。
func (s *Server) pickTemplate(host *db.Host, kind, tmplName string) (db.Template, bool) {
	if tmplName != "" {
		var t db.Template
		if s.db.Where("name = ?", tmplName).First(&t).Error == nil {
			return t, true
		}
	}
	if host != nil {
		var t db.Template
		if s.db.Where("scope = ? AND owner_id = ?", db.ScopeHost, host.ID).Order("priority desc").First(&t).Error == nil {
			return t, true
		}
		var u db.Template
		if s.db.Where("scope = ? AND owner_id = ?", db.ScopeUser, host.UserID).Order("priority desc").First(&u).Error == nil {
			return u, true
		}
	}
	var p db.Template
	if s.db.Where("scope = ? AND kind = ?", db.ScopePublic, kind).Order("priority desc").First(&p).Error == nil {
		return p, true
	}
	if s.db.Where("scope = ?", db.ScopePublic).Order("priority desc").First(&p).Error == nil {
		return p, true
	}
	return db.Template{}, false
}

// handleContent 提供按 host/user/global/base 合并后的镜像文件。
// URL: /c/<mac>/<isoname>/<path...>
func (s *Server) handleContent(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/c/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	mac := normMac(parts[0])
	isoName := parts[1]
	rel := ""
	if len(parts) == 3 {
		rel = parts[2]
	}
	hostID, userID := s.lookupIDs(mac)
	file, ok := iso.Resolve(s.cfg.OverlayDir(), s.cfg.CacheDir(), isoName, hostID, userID, rel)
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, file)
}

// handleRaw 直接流式返回原始 ISO 文件（供 sanboot 等原生启动）。
// URL: /r/<mac>/<isoname>
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/r/")
	isoName := strings.SplitN(rest, "/", 2)[0]
	if isoName == "" {
		http.NotFound(w, r)
		return
	}
	var isoRec db.ISO
	if s.db.Where("name = ?", isoName).First(&isoRec).Error != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.cfg.IsosDir(), isoRec.Filename))
}

// handlePe 提供 PE 支撑文件（wimboot/bootmgr.exe/bcd/boot.sdi），供 wimboot 启动 WIM。
// URL: /pe/<path...>
func (s *Server) handlePe(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/pe/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.cfg.PeDir(), rest))
}

func (s *Server) lookupIDs(mac string) (*uint, *uint) {
	if mac == "" {
		return nil, nil
	}
	var h db.Host
	if s.db.Where("mac = ?", mac).First(&h).Error != nil {
		return nil, nil
	}
	hid := h.ID
	uid := h.UserID
	return &hid, &uid
}

func normMac(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", ":")
	return s
}

func writeScript(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}
