package guest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newSvc(now *time.Time) *GuestService {
	return NewGuestService(Options{
		Secret: "0123456789abcdef0123", TTL: 24 * time.Hour, TokensPerHour: 3,
		Now: func() time.Time { return *now },
	})
}

func TestIssueAndVerify(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	g := newSvc(&now)
	tok, err := g.Issue(context.Background(), "198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	if !tok.ExpiresAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatal(tok.ExpiresAt)
	}
	p, err := g.Principal(tok.Token, "198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Owner, "gst_") || !strings.HasPrefix(p.IPKey, "ip_") || p.IPKey != g.IPKey("198.51.100.7") {
		t.Fatalf("%+v", p)
	}
	// Same token from another IP: another quota key, other IP key.
	p2, _ := g.Principal(tok.Token, "203.0.113.9")
	if p2.Owner == p.Owner || p2.IPKey == p.IPKey {
		t.Fatal("keys must depend on the IP")
	}
	// IPv6 counts per /64.
	if g.IPKey("2001:db8:1:2::1") != g.IPKey("2001:db8:1:2:ffff::9") || g.IPKey("2001:db8:1:2::1") == g.IPKey("2001:db8:1:3::1") {
		t.Fatal("IPv6 /64 bucketing")
	}

	// Tampered, foreign-secret and expired tokens fail.
	if _, err := g.Principal(tok.Token+"x", "198.51.100.7"); !errors.Is(err, ErrBadToken) {
		t.Fatal("tampered accepted")
	}
	other := NewGuestService(Options{Secret: "another-secret-value-123", Now: func() time.Time { return now }})
	if _, err := other.Principal(tok.Token, "198.51.100.7"); err == nil {
		t.Fatal("token from another secret accepted")
	}
	now = now.Add(24 * time.Hour)
	if _, err := g.Principal(tok.Token, "198.51.100.7"); !errors.Is(err, ErrBadToken) {
		t.Fatal("expired accepted")
	}
}

func TestIssueRateLimitPerIP(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	g := newSvc(&now)
	ctx := context.Background()
	for i := range 3 {
		if _, err := g.Issue(ctx, "198.51.100.7"); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := g.Issue(ctx, "198.51.100.7"); !errors.Is(err, ErrRateLimited) {
		t.Fatal("4th token in an hour:", err)
	}
	if _, err := g.Issue(ctx, "198.51.100.8"); err != nil {
		t.Fatal("other IP limited:", err)
	}
	now = now.Add(20 * time.Minute)
	if _, err := g.Issue(ctx, "198.51.100.7"); err != nil {
		t.Fatal("refilled:", err)
	}
}

func TestUnconfigured(t *testing.T) {
	g := NewGuestService(Options{})
	if g.Configured() {
		t.Fatal()
	}
	if _, err := g.Issue(context.Background(), "1.2.3.4"); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
}
