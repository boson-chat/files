package integration

import (
	"strings"
	"testing"

	"github.com/boson-chat/files/config"
)

// Guests: each token has the guest quota, and one IP's tokens together get
// at most guest quota × 3.
func TestGuestPerIPCap(t *testing.T) {
	e := newEnv(t, nil, func(c *config.Config) { c.FilesConfig.GuestQuota, c.FilesConfig.GuestMax = "1000", "1000" })
	body := []byte(strings.Repeat("g", 1000))
	const ipA, ipB = "198.51.100.10", "203.0.113.20"

	var tokens []string
	for range 4 {
		tokens = append(tokens, e.guestToken(ipA))
	}
	for i, tok := range tokens[:3] {
		if r := e.upload(tok, "g.txt", "text/plain", body, "X-Forwarded-For", ipA); r.status != 201 {
			t.Fatalf("token %d: %d %s", i, r.status, r.body)
		}
	}
	// Per token: the guest quota.
	r := e.upload(tokens[0], "g.txt", "text/plain", []byte("x"), "X-Forwarded-For", ipA)
	if r.status != 413 || num(r.json(t)["quota"]) != 1000 {
		t.Fatalf("per-token cap: %d %s", r.status, r.body)
	}
	// Per IP: a fresh token from the same address is still capped at 3×.
	r = e.upload(tokens[3], "g.txt", "text/plain", body, "X-Forwarded-For", ipA)
	if r.status != 413 || r.json(t)["error"] != "quota_exceeded" || num(r.json(t)["quota"]) != 3000 || num(r.json(t)["used"]) != 3000 {
		t.Fatalf("per-IP cap: %d %s", r.status, r.body)
	}
	// Another address is unaffected.
	if r := e.upload(e.guestToken(ipB), "g.txt", "text/plain", body, "X-Forwarded-For", ipB); r.status != 201 {
		t.Fatalf("other IP: %d %s", r.status, r.body)
	}
	// Guest max_file.
	if r := e.upload(e.guestToken(ipB), "big", "text/plain", []byte(strings.Repeat("g", 1001)), "X-Forwarded-For", ipB); r.status != 413 || r.json(t)["error"] != "file_too_large" {
		t.Fatalf("guest max: %d %s", r.status, r.body)
	}
	// Guest usage.
	u := e.public("GET", "/v1/me/usage", nil, "Authorization", tokens[1], "X-Forwarded-For", ipA).json(t)
	if num(u["used"]) != 1000 || num(u["quota"]) != 1000 || num(u["max_file"]) != 1000 {
		t.Fatalf("guest usage %v", u)
	}
}

func TestGuestTokenRateLimitAndSwitch(t *testing.T) {
	e := newEnv(t, nil, func(c *config.Config) { c.GuestConfig.TokensPerHour = 2 })
	for range 2 {
		e.guestToken("192.0.2.1")
	}
	r := e.public("POST", "/v1/guest-token", nil, "X-Forwarded-For", "192.0.2.1")
	if r.status != 429 || r.header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d %s", r.status, r.body)
	}
	r = e.public("POST", "/v1/guest-token", nil, "X-Forwarded-For", "192.0.2.2")
	if r.status != 201 {
		t.Fatal("other IP limited")
	}
	body := r.json(t)
	if _, ok := body["expires_at"]; !ok {
		t.Fatal(body)
	}

	// Guests turned off: no tokens, and existing tokens can't upload.
	tok := e.guestToken("192.0.2.3")
	s := e.app.Settings.Get()
	s.GuestEnabled = false
	e.app.Settings.SetFile(&s)
	if r := e.public("POST", "/v1/guest-token", nil, "X-Forwarded-For", "192.0.2.4"); r.status != 403 || r.json(t)["error"] != "guest_disabled" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if r := e.upload(tok, "g", "text/plain", []byte("x"), "X-Forwarded-For", "192.0.2.3"); r.status != 403 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if c := e.public("GET", "/v1/config", nil).json(t); c["guest_enabled"] != false || c["enabled"] != true {
		t.Fatal(c)
	}
}

func TestGuestsOffWithoutSecret(t *testing.T) {
	e := newEnv(t, nil, func(c *config.Config) { c.GuestConfig.Secret = "" })
	if c := e.public("GET", "/v1/config", nil).json(t); c["guest_enabled"] != false {
		t.Fatal(c)
	}
	if r := e.public("POST", "/v1/guest-token", nil); r.status != 403 {
		t.Fatal(r.status)
	}
}
