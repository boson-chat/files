package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/internal/services/storage"
)

func TestInternalAPI(t *testing.T) {
	store := storage.NewMemStorage()
	e := newEnv(t, store, func(c *config.Config) { c.FilesConfig.AccountQuota = "10KB" })
	auth := e.bearer("usr_judy")
	var ids []string
	for i, name := range []string{"alpha.txt", "beta.txt", "gamma.txt"} {
		r := e.upload(auth, name, "text/plain", []byte(strings.Repeat("j", 100*(i+1))))
		ids = append(ids, r.json(t)["id"].(string))
		e.clock.Advance(time.Minute)
	}
	e.upload(e.bearer("usr_other"), "other.txt", "text/plain", []byte("o"))

	// Token required.
	if r := e.do(e.internal.URL, "GET", "/internal/files/status", nil); r.status != 401 {
		t.Fatal(r.status)
	}
	if r := e.do(e.internal.URL, "GET", "/internal/files/status", nil, "X-Internal-Token", "wrong"); r.status != 401 {
		t.Fatal(r.status)
	}
	st := e.admin("GET", "/internal/files/status", nil).json(t)
	if st["ok"] != true {
		t.Fatal(st)
	}
	totals := st["totals"].(map[string]any)
	if num(totals["live_files"]) != 4 || num(totals["live_bytes"]) != 601 || num(totals["window_users"]) != 2 {
		t.Fatal(totals)
	}
	if st["storage"].(map[string]any)["driver"] != "mem" || st["config"].(map[string]any)["source"] != "env" {
		t.Fatal(st)
	}

	// Search: by user, by name, paging.
	l := e.admin("GET", "/internal/files?user=usr_judy&limit=2", nil).json(t)
	page := l["files"].([]any)
	if len(page) != 2 || page[0].(map[string]any)["name"] != "gamma.txt" || l["next_cursor"] == nil {
		t.Fatal(l)
	}
	l2 := e.admin("GET", "/internal/files?user=usr_judy&limit=2&cursor="+l["next_cursor"].(string), nil).json(t)
	if p := l2["files"].([]any); len(p) != 1 || p[0].(map[string]any)["name"] != "alpha.txt" || l2["next_cursor"] != nil {
		t.Fatal(l2)
	}
	if l := e.admin("GET", "/internal/files?q=BET", nil).json(t)["files"].([]any); len(l) != 1 || l[0].(map[string]any)["user"] != "usr_judy" {
		t.Fatal(l)
	}

	// Moderation delete (no refund either).
	if r := e.admin("DELETE", "/internal/files/"+ids[0], nil); r.status != 204 {
		t.Fatal(r.status, string(r.body))
	}
	if r := e.admin("DELETE", "/internal/files/"+ids[0], nil); r.status != 404 {
		t.Fatal("double delete", r.status)
	}
	all := e.admin("GET", "/internal/files?user=usr_judy&status=all", nil).json(t)["files"].([]any)
	if len(all) != 3 || all[2].(map[string]any)["status"] != "deleted" || all[2].(map[string]any)["deleted_by"] != "admin:mod" {
		t.Fatal(all)
	}

	// Usage + override.
	u := e.admin("GET", "/internal/files/usage?user=usr_judy", nil).json(t)
	if num(u["used"]) != 600 || num(u["quota"]) != 10000 || num(u["live_files"]) != 2 || u["override"] != nil {
		t.Fatal(u)
	}
	u = e.admin("PUT", "/internal/quota/usr_judy", []byte(`{"quota_bytes": 650, "max_file_bytes": null}`)).json(t)
	if num(u["quota"]) != 650 || u["override"] == nil || num(u["max_file"]) != 100e6 {
		t.Fatal(u)
	}
	if r := e.upload(auth, "x.txt", "text/plain", []byte(strings.Repeat("x", 100))); r.status != 413 || num(r.json(t)["quota"]) != 650 {
		t.Fatal("override not applied", r.status, string(r.body))
	}
	if r := e.admin("PUT", "/internal/quota/usr_judy", []byte(`{"quota_bytes": -1}`)); r.status != 400 {
		t.Fatal(r.status)
	}
	u = e.admin("PUT", "/internal/quota/usr_judy", []byte(`null`)).json(t)
	if num(u["quota"]) != 10000 || u["override"] != nil {
		t.Fatal(u)
	}

	// Config file status (no token, like the other services).
	if r := e.do(e.internal.URL, "GET", "/internal/config/status", nil); r.status != 200 || r.json(t)["enabled"] != false {
		t.Fatal(string(r.body))
	}
}

func TestInternalAPIDisabledWithoutToken(t *testing.T) {
	e := newEnv(t, nil, func(c *config.Config) { c.InternalConfig.Token = "" })
	if r := e.do(e.internal.URL, "GET", "/internal/files/status", nil, "X-Internal-Token", "x"); r.status != 503 {
		t.Fatal(r.status)
	}
}

func TestStorageFullS3Capacity(t *testing.T) {
	e := newEnv(t, nil, func(c *config.Config) {
		c.FilesConfig.StorageCapacity, c.FilesConfig.StorageMinFree, c.FilesConfig.AccountQuota = "5000", "1000", "1MB"
	})
	auth := e.bearer("usr_kim")
	if r := e.upload(auth, "a", "text/plain", []byte(strings.Repeat("a", 3000))); r.status != 201 {
		t.Fatal(r.status, string(r.body))
	}
	r := e.upload(auth, "b", "text/plain", []byte(strings.Repeat("b", 1500)))
	if r.status != 507 || r.json(t)["error"] != "storage_full" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if r := e.upload(auth, "c", "text/plain", []byte(strings.Repeat("c", 1000))); r.status != 201 {
		t.Fatal("fits exactly:", r.status, string(r.body))
	}
	st := e.admin("GET", "/internal/files/status", nil).json(t)["storage"].(map[string]any)
	if num(st["used_bytes"]) != 4000 || num(st["free_bytes"]) != 1000 || num(st["total_bytes"]) != 5000 || st["full"] != false {
		t.Fatal(st)
	}
}

func TestStorageFullLocalVolume(t *testing.T) {
	dir := t.TempDir()
	local, err := storage.NewLocalStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	// No volume has an exabyte free.
	e := newEnv(t, local, func(c *config.Config) { c.FilesConfig.StorageMinFree = "1000000TB" })
	if r := e.upload(e.bearer("usr_lee"), "a", "text/plain", []byte("x")); r.status != 507 {
		t.Fatal(r.status, string(r.body))
	}
	st := e.admin("GET", "/internal/files/status", nil).json(t)["storage"].(map[string]any)
	if st["driver"] != "local" || num(st["free_bytes"]) <= 0 || st["full"] != true {
		t.Fatal(st)
	}
}

// The files: section of BOSON_CONFIG reloads live.
func TestConfigFileReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boson.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nfiles:\n  account: { quota: 2KB, max_file: 1KB }\n  retention: 24h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, nil, func(c *config.Config) { c.AppConfig.ConfigFile = path })
	c := e.public("GET", "/v1/config", nil).json(t)
	limits := c["limits"].(map[string]any)
	if num(limits["account"].(map[string]any)["quota_bytes"]) != 2000 || num(c["retention_seconds"]) != 86400 ||
		num(limits["guest"].(map[string]any)["max_file_bytes"]) != 25e6 {
		t.Fatal(c)
	}
	_ = os.WriteFile(path+".tmp", []byte("version: 1\nfiles:\n  enabled: false\n"), 0o644)
	_ = os.Rename(path+".tmp", path)
	e.app.StackConfig.Check(t.Context())
	if c := e.public("GET", "/v1/config", nil).json(t); c["enabled"] != false {
		t.Fatal(c)
	}
	if r := e.upload(e.bearer("usr_max"), "a", "text/plain", []byte("x")); r.status != 503 || r.json(t)["error"] != "uploads_disabled" {
		t.Fatal(r.status, string(r.body))
	}
	st := e.admin("GET", "/internal/files/status", nil).json(t)
	if st["config"].(map[string]any)["source"] != "file" || st["config_file"].(map[string]any)["present"] != true {
		t.Fatal(st)
	}
}
