package management

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestBankedResetsConfigDefaultPersistenceETagReload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("config-version: 8\nserver: {port: 8317}\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Claude.AutoApplyBankedResets {
		t.Fatal("enabled by default")
	}
	h := NewHandler(cfg, path, nil)
	reloads := make(chan *config.Config, 8)
	h.SetConfigReloadHook(func(_ context.Context, c *config.Config) { reloads <- c })
	r := gin.New()
	r.GET("/config", h.ConfigV8)
	r.GET("/config/*path", h.ConfigV8)
	r.PUT("/config/*path", h.ConfigV8)
	req := func(method, url, body, etag string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		q := httptest.NewRequest(method, url, strings.NewReader(body))
		if etag != "" {
			q.Header.Set("If-Match", etag)
		}
		r.ServeHTTP(w, q)
		return w
	}
	w := req("GET", "/config", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var result struct {
		Upstream struct {
			Claude struct {
				Enabled *bool `json:"auto-apply-banked-resets"`
			} `json:"claude"`
		} `json:"upstream"`
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Upstream.Claude.Enabled == nil || *result.Upstream.Claude.Enabled {
		t.Fatal("full JSON did not explicitly advertise false")
	}
	saved, _ := os.ReadFile(path)
	if string(saved) != string(raw) {
		t.Fatal("GET changed YAML")
	}
	etag := w.Header().Get("ETag")
	endpoint := "/config/upstream/claude/auto-apply-banked-resets"
	if w = req("PUT", endpoint, "true", `"stale"`); w.Code != 412 {
		t.Fatal("stale ETag accepted")
	}
	for _, body := range []string{`"true"`, `123`, `{}`, `[]`} {
		if w = req("PUT", endpoint, body, etag); w.Code < 400 {
			t.Fatalf("accepted %s", body)
		}
	}
	if w = req("PUT", endpoint, "true", etag); w.Code != 200 {
		t.Fatalf("write failed %d %s", w.Code, w.Body.String())
	}
	next := <-reloads
	if !next.Claude.AutoApplyBankedResets {
		t.Fatal("reload lost true")
	}
	loaded, err := config.LoadConfig(path)
	if err != nil || !loaded.Claude.AutoApplyBankedResets {
		t.Fatal("true not persisted")
	}
	if w = req("PUT", endpoint, "false", w.Header().Get("ETag")); w.Code != 200 {
		t.Fatal("false failed")
	}
	next = <-reloads
	if next.Claude.AutoApplyBankedResets {
		t.Fatal("false not reloaded")
	}
	loaded, err = config.LoadConfig(path)
	if err != nil || loaded.Claude.AutoApplyBankedResets {
		t.Fatal("false not persisted")
	}
}
func TestBankedResetsAccountAndSessionGuards(t *testing.T) {
	a := &coreauth.Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"access_token": "test-not-real"}}
	if !bankedOAuth(a) {
		t.Fatal("valid OAuth rejected")
	}
	for _, mutate := range []func(*coreauth.Auth){func(b *coreauth.Auth) { b.Disabled = true }, func(b *coreauth.Auth) { b.Status = coreauth.StatusDisabled }, func(b *coreauth.Auth) { b.Provider = "codex" }, func(b *coreauth.Auth) { b.Attributes = map[string]string{"api_key": "test"} }, func(b *coreauth.Auth) { b.Metadata = nil }, func(b *coreauth.Auth) { b.CredentialVersion++ }, func(b *coreauth.Auth) { b.RegistrationEpoch++ }, func(b *coreauth.Auth) { b.Metadata["access_token"] = "replacement" }, func(b *coreauth.Auth) { b.ProxyURL = "http://changed" }} {
		b := a.Clone()
		mutate(b)
		if sameBankedSession(a, b) {
			t.Fatal("changed/disabled session accepted")
		}
	}
	if sameBankedSession(a, nil) {
		t.Fatal("removed account accepted")
	}
	// A nil engine would panic if a default-off tick reached preflight or the journal.
	h := &Handler{cfg: &config.Config{}}
	h.bankedResetTick(context.Background())
	h = NewHandler(&config.Config{}, filepath.Join(t.TempDir(), "config.yaml"), nil)
	h.StartBankedResets()
	h.StartBankedResets()
	h.StopBankedResets()
	h.StopBankedResets()
}
