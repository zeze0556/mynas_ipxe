package httpd

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ipxe-server/config"
	"ipxe-server/internal/db"
)

// TestInitiatorIQNRenderAndUpdate 覆盖：
// 1) Template.InitiatorIQN 经 AutoMigrate 落库；
// 2) /boot 用 {{.InitiatorIQN}} 渲染出 set initiator-iqn；
// 3) PUT /api/templates?id= 能更新 initiator_iqn 且不清空 body / iscsi_target。
func TestInitiatorIQNRenderAndUpdate(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		HTTPPort: 18080, NextServer: "127.0.0.1",
		DataDir: dir, DBPath: filepath.Join(dir, "s.db"), AssetsDir: filepath.Join(dir, "assets"),
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

	isoRec := db.ISO{Name: "debian-live", Filename: "debian-live.iso"}
	if err := gdb.Create(&isoRec).Error; err != nil {
		t.Fatal(err)
	}

	const target = "iscsi:192.168.1.10::::iqn.2024-01.com.example:nas:disk01"
	const initiator = "iqn.2024-01.com.example:nas:disk01:disk01"
	body := "#!ipxe\nset initiator-iqn {{.InitiatorIQN}}\nsanhook {{.ISCSITarget}}\nboot\n"
	tmpl := db.Template{
		Name: "iqn-test", Scope: db.ScopePublic, Kind: db.KindISCSI,
		ISOID: &isoRec.ID, Body: body, ISCSITarget: target, InitiatorIQN: initiator,
	}
	if err := gdb.Create(&tmpl).Error; err != nil {
		t.Fatal(err)
	}

	srv := New(cfg, gdb)

	// 1) 字段确实落库（AutoMigrate 加列生效）
	var got db.Template
	if err := gdb.Where("name = ?", "iqn-test").First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.InitiatorIQN != initiator {
		t.Fatalf("InitiatorIQN not persisted: %q", got.InitiatorIQN)
	}

	// 2) /boot 渲染
	w := httptest.NewRecorder()
	srv.handleBoot(w, httptest.NewRequest("GET", "/boot?template=iqn-test", nil))
	out := w.Body.String()
	if !strings.Contains(out, "set initiator-iqn "+initiator) {
		t.Fatalf("initiator-iqn not rendered:\n%s", out)
	}
	if !strings.Contains(out, "sanhook "+target) {
		t.Fatalf("iscsi target not rendered:\n%s", out)
	}

	// 3) PUT 更新 initiator_iqn，其他字段保持不变
	const initiator2 = "iqn.2024-01.com.example:nas:disk01:client2"
	// 注意：PUT 采用整行 map 覆盖，未提供的字段（如 iso_id）会被清空，故必须带全字段。
	patch := map[string]any{
		"name": "iqn-test", "kind": "iscsi", "scope": "public",
		"iso_id": isoRec.ID, "body": body,
		"iscsi_target": target, "initiator_iqn": initiator2,
	}
	b, _ := json.Marshal(patch)
	w2 := httptest.NewRecorder()
	srv.handleTemplates(w2, httptest.NewRequest("PUT",
		"/api/templates?id="+strconv.FormatUint(uint64(tmpl.ID), 10), bytes.NewReader(b)))
	if w2.Code != 200 {
		t.Fatalf("PUT failed: %d %s", w2.Code, w2.Body.String())
	}
	var after db.Template
	if err := gdb.Where("name = ?", "iqn-test").First(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.InitiatorIQN != initiator2 || after.Body != body || after.ISCSITarget != target {
		t.Fatalf("PUT clobbered fields: %+v", after)
	}
	w3 := httptest.NewRecorder()
	srv.handleBoot(w3, httptest.NewRequest("GET", "/boot?template=iqn-test", nil))
	if !strings.Contains(w3.Body.String(), "set initiator-iqn "+initiator2) {
		t.Fatalf("updated initiator-iqn not rendered:\n%s", w3.Body.String())
	}

	// 4) GET /api/templates 暴露 initiator_iqn 字段
	w4 := httptest.NewRecorder()
	srv.handleTemplates(w4, httptest.NewRequest("GET", "/api/templates", nil))
	if !strings.Contains(w4.Body.String(), `"initiator_iqn":"`+initiator2+`"`) {
		t.Fatalf("initiator_iqn missing from API list: %s", w4.Body.String())
	}
}
