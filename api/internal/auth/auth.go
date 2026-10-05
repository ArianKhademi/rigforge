// Package auth verifies RS256 JWTs against the issuer's JWKS and exposes the
// authenticated user (the "sub" claim) to handlers.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/ArianKhademi/rigforge/api/internal/httpx"
)

const userKey = "user_id"

var ErrInvalidToken = errors.New("invalid token")

// Verifier validates bearer tokens. It keeps the issuer's public keys in
// memory and refetches them when a token names a key id it has not seen,
// which is what makes key rotation at the issuer work without a restart.
type Verifier struct {
	jwksURL  string
	issuer   string
	audience string

	// minRefetch rate-limits JWKS fetches, so a flood of tokens with random
	// key ids cannot turn the api into a request amplifier against the issuer.
	minRefetch time.Duration

	mu        sync.Mutex
	keys      jwk.Set
	fetchedAt time.Time
}

func NewVerifier(jwksURL, issuer, audience string) *Verifier {
	return &Verifier{jwksURL: jwksURL, issuer: issuer, audience: audience, minRefetch: 10 * time.Second}
}

// Verify checks signature, issuer, audience and expiry and returns the subject.
func (v *Verifier) Verify(ctx context.Context, token string) (string, error) {
	// Read the key id from the (still untrusted) header to pick the key.
	msg, err := jws.Parse([]byte(token))
	if err != nil || len(msg.Signatures()) != 1 {
		return "", ErrInvalidToken
	}
	kid, ok := msg.Signatures()[0].ProtectedHeaders().KeyID()
	if !ok || kid == "" {
		return "", ErrInvalidToken
	}
	key, err := v.key(ctx, kid)
	if err != nil {
		return "", err
	}

	// The algorithm is pinned to RS256 here rather than read from the token.
	// Trusting the token's own "alg" header is the classic JWT bug: an attacker
	// switches it to HS256 and signs with the public key as the HMAC secret.
	tok, err := jwt.Parse([]byte(token),
		jwt.WithKey(jwa.RS256(), key),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithAcceptableSkew(30*time.Second),
	)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	sub, ok := tok.Subject()
	if !ok || sub == "" {
		return "", fmt.Errorf("%w: missing sub", ErrInvalidToken)
	}
	return sub, nil
}

func (v *Verifier) key(ctx context.Context, kid string) (jwk.Key, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.keys != nil {
		if key, ok := v.keys.LookupKeyID(kid); ok {
			return key, nil
		}
		if time.Since(v.fetchedAt) < v.minRefetch {
			return nil, fmt.Errorf("%w: unknown key id", ErrInvalidToken)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	set, err := jwk.Fetch(ctx, v.jwksURL)
	if err != nil {
		// Not the caller's fault: surfaced as 503 by the middleware.
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	v.keys, v.fetchedAt = set, time.Now()

	if key, ok := set.LookupKeyID(kid); ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: unknown key id", ErrInvalidToken)
}

// Middleware rejects requests without a valid bearer token and stores the
// subject on the context for handlers.
func Middleware(v *Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || raw == "" {
			c.Header("WWW-Authenticate", `Bearer realm="rigforge"`)
			httpx.Error(c, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		sub, err := v.Verify(c.Request.Context(), raw)
		switch {
		case errors.Is(err, ErrInvalidToken):
			c.Header("WWW-Authenticate", `Bearer realm="rigforge", error="invalid_token"`)
			httpx.Error(c, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
			return
		case err != nil:
			httpx.Error(c, http.StatusServiceUnavailable, "auth_unavailable", "cannot reach token issuer")
			return
		}
		c.Set(userKey, sub)
		c.Next()
	}
}

// UserID returns the authenticated subject. It is only called on routes behind
// Middleware, where the value is always set.
func UserID(c *gin.Context) string {
	return c.GetString(userKey)
}
