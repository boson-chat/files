package middleware

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	nethttp "net/http"
	"strings"
	"time"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/internal/apperr"
	"github.com/boson-chat/files/internal/jwks"
	"github.com/boson-chat/files/internal/logger"
	"github.com/boson-chat/files/internal/principal"
	"github.com/boson-chat/files/internal/reqmeta"
	"github.com/golang-jwt/jwt/v5"
)

// InternalTokenHeader authenticates the admin panel on the internal listener.
const (
	InternalTokenHeader = "X-Internal-Token"
	ActingUserHeader    = "X-Acting-User"
)

var (
	ErrUnauthenticated = apperr.New(nethttp.StatusUnauthorized, "unauthenticated", "missing or invalid credentials")
	errNoAccounts      = apperr.New(nethttp.StatusUnauthorized, "accounts_disabled", "account uploads are not configured on this files host (AUTH_ISSUER)")
	errInternalOff     = apperr.New(nethttp.StatusServiceUnavailable, "internal_api_disabled", "INTERNAL_TOKEN is not set")
)

// AccessClaims are the claims of an auth-service access token.
type AccessClaims struct {
	jwt.RegisteredClaims
	SessionID string `json:"sid"`
	Name      string `json:"name"`
}

// TokenVerifier checks auth-service access tokens.
type TokenVerifier interface {
	Verify(token string) (*AccessClaims, error)
}

// JWTVerifier checks access tokens: EdDSA only, a key from the JWKS
// (cached, refetched on an unknown kid), typ at+jwt, and exp/iss/aud.
type JWTVerifier struct {
	keys     jwt.Keyfunc
	ready    func() bool
	issuer   string
	audience string
}

// NewJWTVerifier starts the JWKS cache. The JWKS being unreachable at
// startup isn't fatal: it's retried in the background (1 s backoff up to
// 30 s) and tokens fail until it loads.
func NewJWTVerifier(ctx context.Context, cfg config.AuthConfig) (*JWTVerifier, error) {
	if cfg.JWKSURL == "" || cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("AUTH_ISSUER, AUTH_JWKS_URL and AUTH_AUDIENCE are required for account uploads")
	}
	k, err := jwks.New(ctx, []string{cfg.JWKSURL}, jwks.Options{Logger: logger.FromCtx(ctx)})
	if err != nil {
		return nil, fmt.Errorf("JWKS %s: %w", cfg.JWKSURL, err)
	}
	v := NewJWTVerifierWithKeyfunc(k.Keyfunc, cfg.Issuer, cfg.Audience)
	v.ready = k.Ready
	return v, nil
}

// KeysLoaded reports whether the JWKS has loaded (always true for a
// verifier built from a Keyfunc).
func (v *JWTVerifier) KeysLoaded() bool {
	return v.ready == nil || v.ready()
}

// NewJWTVerifierWithKeyfunc is NewJWTVerifier with the keys supplied (tests).
func NewJWTVerifierWithKeyfunc(kf jwt.Keyfunc, issuer, audience string) *JWTVerifier {
	return &JWTVerifier{keys: kf, issuer: issuer, audience: audience}
}

func (v *JWTVerifier) Verify(token string) (*AccessClaims, error) {
	var c AccessClaims
	t, err := jwt.ParseWithClaims(token, &c, v.keys,
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return nil, err
	}
	if typ, _ := t.Header["typ"].(string); !strings.EqualFold(typ, "at+jwt") {
		return nil, errors.New("not an access token")
	}
	if !strings.HasPrefix(c.Subject, "usr_") {
		return nil, errors.New("token sub is not an auth user id")
	}
	return &c, nil
}

// GuestVerifier checks guest tokens (internal/services/guest).
type GuestVerifier interface {
	Principal(token, ip string) (*principal.Principal, error)
}

// Authenticate resolves "Authorization: Bearer <access token>" (account)
// or "Authorization: Guest <token>" (guest) and stores the principal. No
// credentials = anonymous (handlers decide); bad credentials = 401.
// accounts may be nil (AUTH_ISSUER unset).
func Authenticate(accounts TokenVerifier, guests GuestVerifier, onError func(nethttp.ResponseWriter, *nethttp.Request, error)) func(nethttp.Handler) nethttp.Handler {
	return func(next nethttp.Handler) nethttp.Handler {
		return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
			authz := strings.TrimSpace(r.Header.Get("Authorization"))
			if authz == "" {
				next.ServeHTTP(w, r)
				return
			}
			scheme, cred, _ := strings.Cut(authz, " ")
			cred = strings.TrimSpace(cred)
			var p *principal.Principal
			switch strings.ToLower(scheme) {
			case "bearer":
				if accounts == nil {
					onError(w, r, errNoAccounts)
					return
				}
				c, err := accounts.Verify(cred)
				if err != nil {
					onError(w, r, ErrUnauthenticated)
					return
				}
				p = &principal.Principal{Kind: principal.KindAccount, Owner: c.Subject, Name: c.Name}
			case "guest":
				gp, err := guests.Principal(cred, reqmeta.From(r.Context()).IP)
				if err != nil {
					onError(w, r, err)
					return
				}
				p = gp
			default:
				onError(w, r, ErrUnauthenticated)
				return
			}
			next.ServeHTTP(w, r.WithContext(principal.With(r.Context(), p)))
		})
	}
}

// RequireInternalToken admits requests carrying X-Internal-Token equal to
// token (constant-time). Empty token: 503 internal_api_disabled.
func RequireInternalToken(token string, onError func(nethttp.ResponseWriter, *nethttp.Request, error), h nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if token == "" {
			onError(w, r, errInternalOff)
			return
		}
		got := r.Header.Get(InternalTokenHeader)
		if got == "" {
			if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
				got = strings.TrimSpace(b)
			}
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			onError(w, r, ErrUnauthenticated)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ActingUser is the admin named by X-Acting-User (for logs), max 64 chars.
func ActingUser(r *nethttp.Request) string {
	a := strings.TrimSpace(r.Header.Get(ActingUserHeader))
	if len(a) > 64 {
		a = a[:64]
	}
	return a
}
