// Package guest issues and checks guest upload tokens: short-lived HMAC
// signed tokens (FILES_GUEST_SECRET), handed out per client IP under a rate
// limit. A guest's quota key is a hash of its client IP and the token id;
// the per-IP total is keyed by a hash of the IP alone (an IPv6 address
// counts as its /64), so one address can't open endless tokens.
package guest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/boson-chat/files/internal/apperr"
	"github.com/boson-chat/files/internal/ids"
	"github.com/boson-chat/files/internal/principal"
	"github.com/boson-chat/files/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"
)

const tokenPrefix = "g1."

var (
	ErrDisabled    = apperr.New(http.StatusForbidden, "guest_disabled", "guest uploads are turned off on this network")
	ErrRateLimited = apperr.New(http.StatusTooManyRequests, "rate_limited", "too many guest tokens from this address; try again later")
	ErrBadToken    = apperr.New(http.StatusUnauthorized, "invalid_guest_token", "the guest token is invalid or expired")
)

// Token is a freshly issued guest token.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type claims struct {
	ID  string `json:"jti"`
	IAT int64  `json:"iat"`
	EXP int64  `json:"exp"`
}

type GuestServiceImpl interface {
	// Configured reports whether a secret is set (guests possible at all).
	Configured() bool
	// Issue mints a token for a client at ip (rate-limited per IP).
	Issue(ctx context.Context, ip string) (Token, error)
	// Principal checks token and returns the guest calling from ip.
	Principal(token, ip string) (*principal.Principal, error)
	// IPKey is the per-IP quota key of ip.
	IPKey(ip string) string
}

type Options struct {
	Secret        string
	TTL           time.Duration
	TokensPerHour int
	Now           func() time.Time
}

type GuestService struct {
	o        Options
	key      []byte
	mu       sync.Mutex
	limiters map[string]*limiterEntry
}

type limiterEntry struct {
	l    *rate.Limiter
	seen time.Time
}

func NewGuestService(o Options) *GuestService {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.TTL <= 0 {
		o.TTL = 24 * time.Hour
	}
	if o.TokensPerHour <= 0 {
		o.TokensPerHour = 10
	}
	return &GuestService{o: o, key: []byte(o.Secret), limiters: map[string]*limiterEntry{}}
}

func (g *GuestService) Configured() bool { return len(g.key) > 0 }

func (g *GuestService) mac(parts ...string) []byte {
	h := hmac.New(sha256.New, g.key)
	h.Write([]byte(strings.Join(parts, "|")))
	return h.Sum(nil)
}

// ipBucket is the address a limit applies to: IPv4 as is, IPv6 as its /64.
func ipBucket(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

func (g *GuestService) IPKey(ip string) string {
	return "ip_" + hex.EncodeToString(g.mac("ip", ipBucket(ip))[:16])
}

func (g *GuestService) allow(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.o.Now()
	b := ipBucket(ip)
	e, ok := g.limiters[b]
	if !ok {
		per := rate.Every(time.Hour / time.Duration(g.o.TokensPerHour))
		e = &limiterEntry{l: rate.NewLimiter(per, g.o.TokensPerHour)}
		g.limiters[b] = e
	}
	e.seen = now
	if len(g.limiters) > 10000 {
		for k, v := range g.limiters {
			if now.Sub(v.seen) > time.Hour {
				delete(g.limiters, k)
			}
		}
	}
	return e.l.AllowN(now, 1)
}

func (g *GuestService) Issue(ctx context.Context, ip string) (Token, error) {
	_, span := tracing.Tracer().Start(ctx, "GuestService.Issue", trace.WithAttributes(attribute.String("service", "guest")))
	defer span.End()
	if !g.Configured() {
		return Token{}, ErrDisabled
	}
	if !g.allow(ip) {
		return Token{}, ErrRateLimited
	}
	now := g.o.Now()
	c := claims{ID: ids.New(ids.Guest), IAT: now.Unix(), EXP: now.Add(g.o.TTL).Unix()}
	payload, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString(g.mac("token", p))
	return Token{Token: tokenPrefix + p + "." + sig, ExpiresAt: time.Unix(c.EXP, 0).UTC()}, nil
}

func (g *GuestService) parse(token string) (claims, error) {
	var c claims
	rest, ok := strings.CutPrefix(token, tokenPrefix)
	if !ok || !g.Configured() || len(token) > 512 {
		return c, errors.New("malformed")
	}
	p, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return c, errors.New("malformed")
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, g.mac("token", p)) {
		return c, errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || json.Unmarshal(raw, &c) != nil || c.ID == "" {
		return c, errors.New("malformed payload")
	}
	if g.o.Now().Unix() >= c.EXP {
		return c, errors.New("expired")
	}
	return c, nil
}

func (g *GuestService) Principal(token, ip string) (*principal.Principal, error) {
	c, err := g.parse(token)
	if err != nil {
		return nil, ErrBadToken
	}
	return &principal.Principal{
		Kind:    principal.KindGuest,
		Owner:   "gst_" + hex.EncodeToString(g.mac("guest", ipBucket(ip), c.ID)[:16]),
		IPKey:   g.IPKey(ip),
		TokenID: c.ID,
	}, nil
}
