package agentsig

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/url"
	"time"
)

type Level string

const (
	Verified   Level = "verified"
	Signed     Level = "signed"
	Unverified Level = "unverified"
)

type Profile int

const (
	WebBotAuth Profile = iota
	Money
)

type Result struct {
	Level    Level
	Label    string
	Domain   string
	Business string
	KeyID    string
}

type Error struct{ Reason string }

func (e *Error) Error() string { return e.Reason }

var ErrUnknownKey = errors.New("unknown key")

type Directory interface {
	Key(ctx context.Context, origin, keyID string) (ed25519.PublicKey, error)
}
type Nonces interface {
	Use(ctx context.Context, keyID, nonce string, until time.Time) (bool, error)
}
type Businesses interface {
	Business(ctx context.Context, domain string) (string, bool, error)
}

type Verifier struct {
	PublicURL  *url.URL
	Keys       Directory
	Nonces     Nonces
	Businesses Businesses
	Now        func() time.Time
}

func (v *Verifier) Check(ctx context.Context, r *http.Request, body []byte, p Profile) (Result, error) {
	return Result{Level: Unverified}, nil
}

type member struct{}

func parseSignatureInput(s string) ([]member, error)                     { return nil, nil }
func signatureBase(r *http.Request, u *url.URL, m member) (string, error) { return "", nil }
func signatureOf(s, label string) ([]byte, error)                         { return nil, nil }

type HTTPDirectory struct{ Client *http.Client }

func (d *HTTPDirectory) Key(ctx context.Context, origin, keyID string) (ed25519.PublicKey, error) {
	return nil, ErrUnknownKey
}
