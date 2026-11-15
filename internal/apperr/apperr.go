// Package apperr defines the client-facing errors services return. The HTTP
// layer renders them as {"error": code, "message": message, "code": code}
// plus any Details fields, with Status. Any other error is an internal
// error and is never shown to clients.
package apperr

import (
	"errors"
	"net/http"
)

type Error struct {
	Status  int
	Code    string
	Message string
	// Details are extra top-level fields of the error body (e.g. used,
	// quota, frees_at for quota_exceeded).
	Details map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// New makes an Error.
func New(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

// With returns a copy of e with details added.
func (e *Error) With(details map[string]any) *Error {
	c := *e
	c.Details = details
	return &c
}

// Invalid is a 400 for a bad field value.
func Invalid(msg string) *Error { return New(http.StatusBadRequest, "invalid_request", msg) }

// As extracts an *Error.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

var (
	ErrUnauthenticated = New(http.StatusUnauthorized, "unauthenticated", "missing or invalid credentials")
	ErrForbidden       = New(http.StatusForbidden, "forbidden", "not allowed")
	ErrNotFound        = New(http.StatusNotFound, "not_found", "not found")
)
