// Package integration runs the service against a real Postgres.
//
// Set TEST_DATABASE_URL to a database the test user can CREATE DATABASE
// from (each test gets its own throwaway database). Without it these tests
// are skipped. Locally: `make dev-up test-integration`.
package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/db"
	fileshttp "github.com/boson-chat/files/http"
	"github.com/boson-chat/files/http/middleware"
	"github.com/boson-chat/files/internal/leader"
	"github.com/boson-chat/files/internal/services/storage"
	"github.com/golang-jwt/jwt/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	issuer        = "https://auth.integration.test"
	internalToken = "internal-test-token-0123456789"
	guestSecret   = "guest-secret-0123456789abcdef"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t        *testing.T
	app      *fileshttp.App
	srv      *httptest.Server
	internal *httptest.Server
	clock    *clock
	key      ed25519.PrivateKey
	store    storage.Storage
	sqlDB    *sql.DB
}

// newDatabase creates a throwaway database and returns its DSN.
func newDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "filestest_" + hex.EncodeToString(b)
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("pgx", base)
		if err == nil {
			_, _ = a.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
			_ = a.Close()
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// baseConfig is the env config every test starts from (contract defaults).
func baseConfig(dsn string) config.Config {
	var cfg config.Config
	cfg.AppConfig = config.AppConfig{APPName: "files", Env: "test", ConfigPoll: time.Hour}
	cfg.DBConfig = config.DBConfig{URL: dsn, MaxConns: 40}
	cfg.HTTPConfig = config.HTTPConfig{PublicURL: "https://files.test", TrustedProxies: "127.0.0.1,::1", UploadTimeout: time.Minute}
	cfg.InternalConfig = config.InternalConfig{Token: internalToken}
	cfg.AuthConfig = config.AuthConfig{Issuer: issuer, Audience: "boson"}
	cfg.GuestConfig = config.GuestConfig{Secret: guestSecret, TokenTTL: 24 * time.Hour, TokensPerHour: 100, IPQuotaFactor: 3}
	cfg.FilesConfig = config.FilesConfig{
		Enabled: true, Retention: 72 * time.Hour, Window: 72 * time.Hour,
		AccountQuota: "5GB", AccountMax: "100MB", GuestEnabled: true, GuestQuota: "500MB", GuestMax: "25MB",
		StorageDriver: "local", StorageMinFree: "0", S3Region: "auto", S3PathStyle: true,
	}
	cfg.WorkerConfig = config.WorkerConfig{ExpiryEnabled: false, ExpiryEvery: time.Minute, PendingTimeout: time.Hour}
	return cfg
}

func newEnv(t *testing.T, store storage.Storage, mutate ...func(*config.Config)) *env {
	t.Helper()
	dsn := newDatabase(t)
	if err := db.MigrateUp(dsn); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig(dsn)
	for _, m := range mutate {
		m(&cfg)
	}
	if store == nil {
		store = storage.NewMemStorage()
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	clk := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	verifier := middleware.NewJWTVerifierWithKeyfunc(func(*jwt.Token) (any, error) { return pub, nil }, issuer, "boson")
	ctx := context.Background()
	app, err := fileshttp.Build(ctx, cfg, fileshttp.Options{
		Now: clk.Now, Storage: store, TokenVerifier: verifier, Locker: leader.Local{}, NoNotify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	srv := httptest.NewServer(app.Public)
	t.Cleanup(srv.Close)
	isrv := httptest.NewServer(app.Internal)
	t.Cleanup(isrv.Close)
	sqlDB, _ := sql.Open("pgx", dsn)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return &env{t: t, app: app, srv: srv, internal: isrv, clock: clk, key: priv, store: store, sqlDB: sqlDB}
}

// token mints an access token for user id sub.
func (e *env) token(sub string) string {
	e.t.Helper()
	now := e.clock.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, middleware.AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: sub, Issuer: issuer, Audience: jwt.ClaimStrings{"boson"},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Name: sub,
	})
	tok.Header["typ"] = "at+jwt"
	s, err := tok.SignedString(e.key)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) bearer(sub string) string { return "Bearer " + e.token(sub) }

type resp struct {
	status int
	header nethttp.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.status, r.body)
	}
	return m
}

// do sends a request; headers are name/value pairs.
func (e *env) do(base, method, path string, body []byte, headers ...string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := nethttp.NewRequest(method, base+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

func (e *env) public(method, path string, body []byte, headers ...string) resp {
	e.t.Helper()
	return e.do(e.srv.URL, method, path, body, headers...)
}

func (e *env) admin(method, path string, body []byte) resp {
	e.t.Helper()
	return e.do(e.internal.URL, method, path, body, "X-Internal-Token", internalToken, "Content-Type", "application/json", "X-Acting-User", "mod")
}

// upload POSTs body as a file.
func (e *env) upload(auth, name, contentType string, body []byte, headers ...string) resp {
	e.t.Helper()
	h := append([]string{"Authorization", auth, "X-Filename", url.PathEscape(name), "Content-Type", contentType}, headers...)
	return e.public("POST", "/v1/files", body, h...)
}

// guestToken gets a guest token as client ip.
func (e *env) guestToken(ip string) string {
	e.t.Helper()
	r := e.public("POST", "/v1/guest-token", nil, "X-Forwarded-For", ip)
	if r.status != nethttp.StatusCreated {
		e.t.Fatalf("guest token: %d %s", r.status, r.body)
	}
	return "Guest " + r.json(e.t)["token"].(string)
}

// filePath turns an absolute file URL into a path on the test server.
func filePath(t *testing.T, u string) string {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return p.EscapedPath()
}

func num(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return -1
}

// zeros is a fast reader of n bytes (contents irrelevant).
type zeros struct{ left int64 }

var zbuf = make([]byte, 1<<20)

func (z *zeros) Read(p []byte) (int, error) {
	if z.left <= 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), z.left, int64(len(zbuf)))
	copy(p[:n], zbuf[:n])
	z.left -= n
	return int(n), nil
}
