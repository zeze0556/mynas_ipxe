package httpd

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ipxe-server/config"
	"ipxe-server/internal/db"
	"ipxe-server/internal/iso"
)

// TestBootAndContent 端到端验证：
// 1) MAC -> Host -> Host 级模板（优先于 public）选取并渲染（需求2/3/4）
// 2) 注入文件按 host 层覆盖 base（需求5/6）
// 3) base 文件正常回退
func TestBootAndContent(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		HTTPPort:   8080,
		NextServer: "192.168.1.10",
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
	defer func() {
		if sqlDB, e := gdb.DB(); e == nil {
			_ = sqlDB.Close()
		}
	}()

	mac := "aa:bb:cc:dd:ee:ff"
	var u db.User
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	var h db.Host
	h = db.Host{MAC: mac, Name: "pc1", UserID: u.ID, Vars: `{"hostname":"pc1"}`}
	if err := gdb.Create(&h).Error; err != nil {
		t.Fatal(err)
	}

	isoName := "ubuntu"
	baseDir := filepath.Join(cfg.CacheDir(), isoName)
	if err := os.MkdirAll(filepath.Join(baseDir, "casper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "casper", "vmlinuz"), []byte("BASE"), 0o644); err != nil {
		t.Fatal(err)
	}
	var isoRec db.ISO
	isoRec = db.ISO{Name: isoName, Filename: "ubuntu.iso", ExtractedPath: baseDir}
	if err := gdb.Create(&isoRec).Error; err != nil {
		t.Fatal(err)
	}

	// host 级自定义模板
	body := "#!ipxe\necho host {{.HostName}} {{.Vars.hostname}}\nkernel {{.ContentURL}}/casper/vmlinuz\n"
	var tmpl db.Template
	tmpl = db.Template{Name: "t1", Scope: db.ScopeHost, OwnerID: &h.ID, Kind: db.KindLinux, ISOID: &isoRec.ID, Body: body, Priority: 1}
	if err := gdb.Create(&tmpl).Error; err != nil {
		t.Fatal(err)
	}

	// host 级注入文件（应覆盖 base）
	injPath := "/autounattend.xml"
	stored := iso.OverlayStoredPath(cfg.OverlayDir(), string(db.InjHost), &h.ID, isoName, injPath)
	if err := os.MkdirAll(filepath.Dir(stored), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored, []byte("INJECTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	var inj db.Injection
	inj = db.Injection{ISOID: isoRec.ID, Scope: db.InjHost, OwnerID: &h.ID, ImagePath: injPath, StoredPath: stored}
	if err := gdb.Create(&inj).Error; err != nil {
		t.Fatal(err)
	}

	srv := New(cfg, gdb)

	// 1) boot 渲染
	w := httptest.NewRecorder()
	srv.handleBoot(w, httptest.NewRequest("GET", "/boot?mac="+mac, nil))
	out := w.Body.String()
	wantURL := "http://192.168.1.10:8080/c/" + mac + "/ubuntu/casper/vmlinuz"
	if !strings.Contains(out, "pc1") || !strings.Contains(out, wantURL) {
		t.Fatalf("boot render unexpected:\n%s", out)
	}

	// 2) overlay 注入层返回 INJECTED
	w2 := httptest.NewRecorder()
	srv.handleContent(w2, httptest.NewRequest("GET", "/c/"+mac+"/ubuntu/autounattend.xml", nil))
	if w2.Body.String() != "INJECTED" {
		t.Fatalf("overlay content = %q, want INJECTED", w2.Body.String())
	}

	// 3) base 文件回退
	w3 := httptest.NewRecorder()
	srv.handleContent(w3, httptest.NewRequest("GET", "/c/"+mac+"/ubuntu/casper/vmlinuz", nil))
	if w3.Body.String() != "BASE" {
		t.Fatalf("base content = %q, want BASE", w3.Body.String())
	}
}
