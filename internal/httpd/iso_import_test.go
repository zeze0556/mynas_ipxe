package httpd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ipxe-server/config"
	"ipxe-server/internal/db"
)

func isoTestEnv(t *testing.T) (*Server, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		HTTPPort:   8080,
		NextServer: "127.0.0.1",
		DataDir:    dir,
		DBPath:     filepath.Join(dir, "s.db"),
		AssetsDir:  filepath.Join(dir, "assets"),
	}
	for _, d := range []string{cfg.IsosDir(), cfg.CacheDir(), cfg.OverlayDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gdb, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, e := gdb.DB(); e == nil {
			_ = sqlDB.Close()
		}
	})
	return New(cfg, gdb), cfg
}

func decodeResp(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON: %v / body=%q", err, w.Body.String())
	}
	return m
}

func isoFromResp(t *testing.T, m map[string]any) (uint, string) {
	t.Helper()
	iso, ok := m["iso"].(map[string]any)
	if !ok {
		t.Fatalf("no iso object in response: %v", m)
	}
	return uint(iso["id"].(float64)), iso["checksum"].(string)
}

// 同名文件重复导入必须更新同一条记录，模板 iso_id 绑定不能断。
func TestISOImportIsIdempotent(t *testing.T) {
	srv, _ := isoTestEnv(t)

	w := httptest.NewRecorder()
	srv.importImage(w, strings.NewReader("AAA"), "disk.img", "", 0)
	if w.Code != 201 {
		t.Fatalf("first import status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	m := decodeResp(t, w)
	if m["action"] != "created" {
		t.Fatalf("action = %v, want created", m["action"])
	}
	id1, cs1 := isoFromResp(t, m)

	// 绑定模板，模拟生产里 templates.iso_id
	tpl := db.Template{Name: "t1", Scope: db.ScopePublic, Kind: db.KindRaw, ISOID: &id1, Body: "#!ipxe\necho x\n"}
	if err := srv.db.Create(&tpl).Error; err != nil {
		t.Fatal(err)
	}

	// 第二次：内容变化 → updated，ID 不变
	w2 := httptest.NewRecorder()
	srv.importImage(w2, strings.NewReader("BBB"), "disk.img", "", 0)
	if w2.Code != 200 {
		t.Fatalf("second import status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	m2 := decodeResp(t, w2)
	if m2["action"] != "updated" {
		t.Fatalf("action = %v, want updated", m2["action"])
	}
	id2, cs2 := isoFromResp(t, m2)
	if id2 != id1 {
		t.Fatalf("ID changed from %d to %d (would break template binding)", id1, id2)
	}
	if cs2 == cs1 {
		t.Fatal("checksum not refreshed")
	}

	var n int64
	srv.db.Model(&db.ISO{}).Count(&n)
	if n != 1 {
		t.Fatalf("iso rows = %d, want 1 (duplicate row created)", n)
	}
	var got db.Template
	if err := srv.db.First(&got, tpl.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ISOID == nil || *got.ISOID != id1 {
		t.Fatalf("template iso_id = %v, want %d", got.ISOID, id1)
	}

	// 第三次：内容相同 → unchanged
	w3 := httptest.NewRecorder()
	srv.importImage(w3, strings.NewReader("BBB"), "disk.img", "", 0)
	m3 := decodeResp(t, w3)
	if m3["action"] != "unchanged" {
		t.Fatalf("action = %v, want unchanged", m3["action"])
	}
	srv.db.Model(&db.ISO{}).Count(&n)
	if n != 1 {
		t.Fatalf("iso rows after unchanged import = %d, want 1", n)
	}
}

// 删除必须挡住悬空引用；force=1 才解绑。
func TestISODeleteReferenceGuard(t *testing.T) {
	srv, _ := isoTestEnv(t)
	w := httptest.NewRecorder()
	srv.importImage(w, strings.NewReader("AAA"), "disk.img", "", 0)
	id, _ := isoFromResp(t, decodeResp(t, w))
	idStr := strconvItoa(uint64(id))

	tpl := db.Template{Name: "bound", Scope: db.ScopePublic, Kind: db.KindRaw, ISOID: &id, Body: "#!ipxe"}
	if err := srv.db.Create(&tpl).Error; err != nil {
		t.Fatal(err)
	}
	inj := db.Injection{ISOID: id, Scope: db.InjGlobal, ImagePath: "/x.xml", StoredPath: "/tmp/x"}
	if err := srv.db.Create(&inj).Error; err != nil {
		t.Fatal(err)
	}

	// 无 force → 409 且引用仍在
	w2 := httptest.NewRecorder()
	srv.handleISOs(w2, httptest.NewRequest(http.MethodDelete, "/api/isos?id="+idStr, nil))
	if w2.Code != 409 {
		t.Fatalf("status = %d, want 409; body=%s", w2.Code, w2.Body.String())
	}
	m2 := decodeResp(t, w2)
	if len(m2["templates"].([]any)) != 1 || len(m2["injections"].([]any)) != 1 {
		t.Fatalf("409 body missing references: %v", m2)
	}
	var n int64
	srv.db.Model(&db.ISO{}).Count(&n)
	if n != 1 {
		t.Fatal("record was deleted despite 409")
	}

	// force=1 → 解绑模板 + 删注入 + 删记录
	w3 := httptest.NewRecorder()
	srv.handleISOs(w3, httptest.NewRequest(http.MethodDelete, "/api/isos?id="+idStr+"&force=1", nil))
	if w3.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w3.Code, w3.Body.String())
	}
	m3 := decodeResp(t, w3)
	if m3["unbound_templates"].(float64) != 1 || m3["deleted_injections"].(float64) != 1 {
		t.Fatalf("unexpected cascade: %v", m3)
	}
	srv.db.Model(&db.ISO{}).Count(&n)
	if n != 0 {
		t.Fatal("iso record not deleted")
	}
	var got db.Template
	srv.db.First(&got, tpl.ID)
	if got.ISOID != nil {
		t.Fatalf("template still bound to deleted iso: %v", *got.ISOID)
	}
	srv.db.Model(&db.Injection{}).Count(&n)
	if n != 0 {
		t.Fatal("injections not cleaned")
	}
}

// 真实 HTTP 链路：按路径导入 → 内容替换 → refresh → 删除。
func TestISOAPIEndToEnd(t *testing.T) {
	srv, cfg := isoTestEnv(t)
	mux := http.NewServeMux()
	srv.registerAPI(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	src := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(src, []byte("V1"), 0o644); err != nil {
		t.Fatal(err)
	}

	postJSON := func(url string, v any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(v)
		resp, err := http.Post(url, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, m
	}

	st1, m1 := postJSON(ts.URL+"/api/isos/import", map[string]string{"path": src})
	if st1 != 201 || m1["action"] != "created" {
		t.Fatalf("first path import: %d %v", st1, m1)
	}
	id1, cs1 := isoFromResp(t, m1)
	idStr := strconvItoa(uint64(id1))

	// 文件被外部替换（同一路径），再次按路径导入
	if err := os.WriteFile(src, []byte("V2-LONGER"), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, m2 := postJSON(ts.URL+"/api/isos/import", map[string]string{"path": src})
	if st2 != 200 || m2["action"] != "updated" {
		t.Fatalf("second path import: %d %v", st2, m2)
	}
	id2, cs2 := isoFromResp(t, m2)
	if id2 != id1 || cs2 == cs1 {
		t.Fatalf("not idempotent: id %d->%d checksum %s->%s", id1, id2, cs1, cs2)
	}
	var n int64
	srv.db.Model(&db.ISO{}).Count(&n)
	if n != 1 {
		t.Fatalf("iso rows = %d, want 1", n)
	}

	// refresh：直接改 isos 目录里的落地文件
	if err := os.WriteFile(filepath.Join(cfg.IsosDir(), "disk.img"), []byte("V3-FROM-REFRESH"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/api/isos/refresh?id="+idStr, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m3 map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m3); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || m3["action"] != "updated" {
		t.Fatalf("refresh: %d %v", resp.StatusCode, m3)
	}
	id3, cs3 := isoFromResp(t, m3)
	if id3 != id1 || cs3 == cs2 {
		t.Fatalf("refresh changed identity or did not refresh checksum")
	}

	// 删除（无引用）→ 200，默认不删磁盘文件
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/isos?id="+idStr, nil)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer del.Body.Close()
	if del.StatusCode != 200 {
		t.Fatalf("delete status = %d", del.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(cfg.IsosDir(), "disk.img")); err != nil {
		t.Fatal("default delete removed the disk file (should keep it)")
	}
}

// 缓存目录原子替换必须清掉旧 ISO 独有的文件，且不留下 backup。
func TestSwapCacheDirDropsStaleFiles(t *testing.T) {
	dir := t.TempDir()
	cacheRoot := filepath.Join(dir, "cache")
	cache := filepath.Join(cacheRoot, "demo")
	tmp := filepath.Join(cacheRoot, "incoming")
	for _, d := range []string{cache, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(cache, "stale.txt"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(tmp, "new.txt"), []byte("new"), 0o644)

	if err := swapCacheDir(tmp, cache); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("stale file survived the swap")
	}
	if _, err := os.Stat(filepath.Join(cache, "new.txt")); err != nil {
		t.Fatal("new content missing after swap:", err)
	}
	entries, _ := os.ReadDir(cacheRoot)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".old-") {
			t.Fatal("backup directory left behind:", e.Name())
		}
	}
}

func strconvItoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// 点「重新导入 #N」必须更新 #N 本身，而不是静默更新同名的最小 id 那条。
func TestISORefreshTargetsRequestedID(t *testing.T) {
	srv, cfg := isoTestEnv(t)

	w := httptest.NewRecorder()
	srv.importImage(w, strings.NewReader("AAA"), "disk.img", "", 0)
	id1, _ := isoFromResp(t, decodeResp(t, w))

	// 模拟存量脏数据：同名、不同 checksum 的第二条记录
	dup := db.ISO{Name: "disk", Filename: "disk.img", Size: 3, Checksum: "STALE", Format: "img"}
	if err := srv.db.Create(&dup).Error; err != nil {
		t.Fatal(err)
	}
	if dup.ID == id1 {
		t.Fatal("test setup broken: duplicate got same id")
	}

	var snap db.ISO
	srv.db.First(&snap, id1)
	checksumBefore := snap.Checksum

	if err := os.WriteFile(filepath.Join(cfg.IsosDir(), "disk.img"), []byte("BBB-NEW"), 0o644); err != nil {
		t.Fatal(err)
	}

	w2 := httptest.NewRecorder()
	srv.handleRefreshISO(w2, httptest.NewRequest(http.MethodPost, "/api/isos/refresh?id="+strconvItoa(uint64(dup.ID)), nil))
	if w2.Code != 200 {
		t.Fatalf("refresh status = %d, body=%s", w2.Code, w2.Body.String())
	}
	m2 := decodeResp(t, w2)
	gotID, gotCS := isoFromResp(t, m2)
	if gotID != dup.ID {
		t.Fatalf("refresh updated #%d instead of requested #%d", gotID, dup.ID)
	}
	if gotCS == "STALE" {
		t.Fatal("requested record was not refreshed")
	}
	dups := m2["duplicate_ids"].([]any)
	if len(dups) != 1 || uint(dups[0].(float64)) != id1 {
		t.Fatalf("duplicate_ids = %v, want [%d]", dups, id1)
	}
	// 未指定的那条不能被改动
	var other db.ISO
	srv.db.First(&other, id1)
	if other.Checksum != checksumBefore {
		t.Fatalf("unrequested record #%d checksum changed: %s -> %s", id1, checksumBefore, other.Checksum)
	}

	// purge=1 但仍有同名记录 → 不许删磁盘文件
	w3 := httptest.NewRecorder()
	srv.handleISOs(w3, httptest.NewRequest(http.MethodDelete, "/api/isos?id="+strconvItoa(uint64(dup.ID))+"&purge=1", nil))
	if w3.Code != 200 {
		t.Fatalf("delete status = %d, body=%s", w3.Code, w3.Body.String())
	}
	if decodeResp(t, w3)["purged"] != false {
		t.Fatal("purged=true while a duplicate record still shares the file")
	}
	if _, err := os.Stat(filepath.Join(cfg.IsosDir(), "disk.img")); err != nil {
		t.Fatal("shared disk file was deleted")
	}
}

// 存量库已有同名重复行时，新二进制启动的 AutoMigrate 不能失败。
func TestAutoMigrateToleratesDuplicateFilenames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.db")
	gdb, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, cs := range []string{"aaa", "bbb"} {
		if err := gdb.Create(&db.ISO{Name: "same", Filename: "same.iso", Checksum: cs, Format: "iso"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(gdb); err != nil {
		t.Fatalf("AutoMigrate failed on a table with duplicate filenames: %v", err)
	}
	if sqlDB, e := gdb.DB(); e == nil {
		_ = sqlDB.Close()
	}
	// 重新打开（真实启动路径也要跑 AutoMigrate）
	gdb2, err := db.Open(path)
	if err != nil {
		t.Fatalf("reopen with duplicate rows failed: %v", err)
	}
	var n int64
	gdb2.Model(&db.ISO{}).Count(&n)
	if n != 2 {
		t.Fatalf("rows after reopen = %d, want 2", n)
	}
	if sqlDB, e := gdb2.DB(); e == nil {
		_ = sqlDB.Close()
	}
}
