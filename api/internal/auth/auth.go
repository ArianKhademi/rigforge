// Package auth verifies RS256 JWTs against the issuer's JWKS and exposes the
// authenticated user (the "sub" claim) to handlers.
//
// Verification is done by jwkit (github.com/ArianKhademi/jwkit), which pins
// the algorithm allow-list (RS256/ES256, never "none" or HMAC), binds each
// JWKS key to one algorithm, checks iss/aud/exp/nbf/iat in a fixed order and
// refetches the JWKS, rate-limited, when a token names an unknown key id.
// This package adds what is specific to the api: the error envelope and the
// user id on the request context.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	jwkit "github.com/ArianKhademi/jwkit/go"
	"github.com/gin-gonic/gin"

	"github.com/ArianKhademi/rigforge/api/internal/httpx"
)

const userKey = "user_id"

var (
	// ErrInvalidToken: the token was rejected (malformed, bad signature,
	// wrong issuer or audience, expired, ...). The reason is wrapped.
	ErrInvalidToken = errors.New("invalid token")
	// ErrUnavailable: the token could not be checked because the issuer's
	// keys could not be fetched and none are cached. Not the caller's fault.
	ErrUnavailable = errors.New("token verification unavailable")
)

type Verifier struct {
	inner *jwkit.Verifier
}

func NewVerifier(jwksURL, issuer, audience string) (*Verifier, error) {
	v, err := jwkit.NewVerifier(jwkit.Config{
		Issuer:   issuer,
		Audience: audience,
		JWKSURL:  jwksURL,
		// A failed refresh keeps the last good key set; just say so.
		OnWarning: func(err error) { slog.Warn("jwks refresh failed", "err", err) },
	})
	if err != nil {
		return nil, err
	}
	return &Verifier{inner: v}, nil
}

// Verify checks the token and returns its subject.
func (v *Verifier) Verify(ctx context.Context, token string) (string, error) {
	claims, err := v.inner.Verify(ctx, token)
	if err != nil {
		// Both are wrapped: callers match on the sentinel, and jwkit.ErrorName
		// can still find the check that failed.
		if errors.Is(err, jwkit.ErrJWKSUnavailable) {
			return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		return "", fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if claims.Subject == "" {
		return "", fmt.Errorf("%w: missing sub", ErrInvalidToken)
	}
	return claims.Subject, nil
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
			// Only the check's name goes to the client (e.g. "ErrExpired");
			// the detail stays in the server log.
			c.Header("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="rigforge", error="invalid_token", error_description=%q`, jwkit.ErrorName(err)))
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
