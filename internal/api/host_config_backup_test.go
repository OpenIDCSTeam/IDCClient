package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/logger"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
)

func TestHostConfigBackupAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	db, err := data.NewDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app := &App{DB: db, HostSvc: service.NewHostService(db), Logger: logger.NewLogger(db, dir)}
	defer app.Logger.Close()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(auth.ContextKeyUser, &auth.UserContext{Username: "test-admin", IsAdmin: c.GetHeader("X-Test-Admin") == "true"})
		c.Next()
	})
	router.GET("/export", auth.RequireAdmin(), app.handleHostConfigExport)
	router.POST("/restore", auth.RequireAdmin(), app.handleHostConfigRestore)
	request := func(method, path string, body []byte, admin bool) (int, *httptest.ResponseRecorder) {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if admin {
			req.Header.Set("X-Test-Admin", "true")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		var result struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatalf("invalid API response: %v", err)
		}
		return result.Code, response
	}
	cfg := model.NewHSConfig()
	cfg.ServerName = "restore-test"
	cfg.ServerType = "PromoxSetup"
	cfg.ServerPass = "test-only-password"
	file := service.HostConfigBackup{Format: service.HostConfigBackupFormat, Version: 1, Hosts: []*model.HSConfig{cfg}}
	body, _ := json.Marshal(map[string]interface{}{"backup": file})

	for _, path := range []string{"/export", "/restore"} {
		method := http.MethodGet
		if path == "/restore" {
			method = http.MethodPost
		}
		if code, _ := request(method, path, body, false); code != 403 {
			t.Fatalf("non-admin can access %s: %d", path, code)
		}
	}
	if code, _ := request(http.MethodPost, "/restore", body, true); code != 200 {
		t.Fatalf("default preview failed: %d", code)
	}
	if actual, _ := app.HostSvc.GetHost(cfg.ServerName); actual != nil {
		t.Fatal("missing dry_run executed restore")
	}
	body, _ = json.Marshal(map[string]interface{}{"backup": file, "dry_run": false})
	if code, _ := request(http.MethodPost, "/restore", body, true); code != 200 {
		t.Fatalf("create restore failed: %d", code)
	}
	if code, _ := request(http.MethodPost, "/restore", body, true); code != 400 {
		t.Fatalf("unconfirmed overwrite accepted: %d", code)
	}
	cfg.ServerPass = ""
	cfg.IPAddrDDNS = []string{}
	body, _ = json.Marshal(map[string]interface{}{"backup": file, "dry_run": false, "overwrite": true})
	if code, _ := request(http.MethodPost, "/restore", body, true); code != 200 {
		t.Fatalf("confirmed overwrite failed: %d", code)
	}
	actual, err := app.HostSvc.GetHost(cfg.ServerName)
	if err != nil || actual == nil || actual.ServerPass != "" || len(actual.IPAddrDDNS) != 0 {
		t.Fatal("API did not restore empty credentials and DNS")
	}
	code, response := request(http.MethodGet, "/export?hs_name=restore-test", nil, true)
	if code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("export failed or can be cached")
	}
	var exported struct {
		Data service.HostConfigBackup `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &exported); err != nil || len(exported.Data.Hosts) != 1 {
		t.Fatal("single-host export invalid")
	}
	if code, _ := request(http.MethodGet, "/export?hs_name=missing", nil, true); code != 400 {
		t.Fatal("missing host export was accepted")
	}
	if code, _ := request(http.MethodPost, "/restore", append(body, []byte("{}")...), true); code != 400 {
		t.Fatal("trailing JSON request accepted")
	}
	if code, _ := request(http.MethodPost, "/restore", bytes.Repeat([]byte(" "), service.MaxHostConfigBackupSize+1025), true); code != 400 {
		t.Fatal("oversized restore request accepted")
	}
}
