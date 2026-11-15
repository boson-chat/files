// Package principal is the authenticated caller of a request: a boson
// account (bearer access token from the auth service) or a guest (a guest
// token from POST /v1/guest-token). Middleware stores it in the context.
package principal

import "context"

type Kind string

const (
	KindAccount Kind = "account"
	KindGuest   Kind = "guest"
)

type Principal struct {
	Kind Kind
	// Owner is the quota key: the auth user id (usr_…) for accounts, the
	// guest key (gst_…, a hash of the client IP and the token id) for guests.
	Owner string
	// IPKey (guests only) is the hashed client IP (ip_…), for the per-IP
	// total.
	IPKey string
	// Name is the account's display name (logs only).
	Name string
	// TokenID is the guest token id.
	TokenID string
}

type ctxKey struct{}

func With(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// From returns the principal, or nil for anonymous requests.
func From(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}
