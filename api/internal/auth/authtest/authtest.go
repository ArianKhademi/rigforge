// Package authtest is a throwaway RS256 issuer for tests: it serves a JWKS
// over httptest and mints tokens, so tests exercise the real verification path.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

const (
	IssuerName = "test-issuer"
	Audience   = "rigforge-api"
)

type Issuer struct {
	Server *httptest.Server
	key    jwk.Key
}

// New starts a JWKS server that is shut down when the test ends.
func New(t testing.TB) *Issuer {
	t.Helper()
	key := NewKey(t, "test-key")
	pub, err := jwk.PublicKeyOf(key)
	if err != nil {
		t.Fatal(err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pub); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(srv.Close)
	return &Issuer{Server: srv, key: key}
}

func (i *Issuer) JWKSURL() string { return i.Server.URL }

// Token mints a token for sub that expires after ttl (negative = already expired).
func (i *Issuer) Token(t testing.TB, sub string, ttl time.Duration) string {
	t.Helper()
	return Sign(t, i.key, IssuerName, Audience, sub, ttl)
}

// TokenWith mints a token signed by the trusted key but with arbitrary issuer
// and audience claims, for testing claim validation in isolation.
func (i *Issuer) TokenWith(t testing.TB, iss, aud, sub string, ttl time.Duration) string {
	t.Helper()
	return Sign(t, i.key, iss, aud, sub, ttl)
}

func NewKey(t testing.TB, kid string) jwk.Key {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key, err := jwk.Import(raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = key.Set(jwk.KeyIDKey, kid)
	_ = key.Set(jwk.AlgorithmKey, jwa.RS256())
	return key
}

func Sign(t testing.TB, key jwk.Key, iss, aud, sub string, ttl time.Duration) string {
	t.Helper()
	now := time.Now()
	tok, err := jwt.NewBuilder().
		Issuer(iss).
		Audience([]string{aud}).
		Subject(sub).
		IssuedAt(now.Add(-time.Minute)).
		Expiration(now.Add(ttl)).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), key))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}
