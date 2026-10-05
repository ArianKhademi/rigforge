// Command issuer is a tiny development token issuer. It exists so the api's
// authentication path is real (RS256 signatures verified against a JWKS)
// without depending on an external identity provider.
//
// It is NOT a login system: anyone who can reach POST /token gets a token for
// any subject they name. In production the api's JWT_JWKS_URL, JWT_ISSUER and
// JWT_AUDIENCE would point at a real IdP instead and this binary is not deployed.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

const tokenTTL = 12 * time.Hour

var subjectPattern = regexp.MustCompile(`^[a-zA-Z0-9_.@-]{1,64}$`)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("issuer exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := envOr("ISSUER_ADDR", ":8090")
	issuer := envOr("JWT_ISSUER", "rigforge-dev-issuer")
	audience := envOr("JWT_AUDIENCE", "rigforge-api")

	key, err := loadKey()
	if err != nil {
		return err
	}
	pub, err := jwk.PublicKeyOf(key)
	if err != nil {
		return err
	}
	jwks := jwk.NewSet()
	if err := jwks.AddKey(pub); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// The api fetches this to verify signatures. It contains public keys only.
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, jwks)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Sub string `json:"sub"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil ||
			!subjectPattern.MatchString(req.Sub) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "sub must be 1-64 characters of letters, digits, '_', '.', '@' or '-'"})
			return
		}
		now := time.Now()
		tok, err := jwt.NewBuilder().
			Issuer(issuer).
			Audience([]string{audience}).
			Subject(req.Sub).
			IssuedAt(now).
			Expiration(now.Add(tokenTTL)).
			Build()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not build token"})
			return
		}
		// jwt.Sign puts the key's "kid" in the header; the api uses it to pick
		// the verification key from the JWKS.
		signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), key))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not sign token"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": string(signed),
			"token_type":   "Bearer",
			"expires_in":   int(tokenTTL.Seconds()),
		})
	})

	slog.Info("dev issuer listening", "addr", addr, "issuer", issuer)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return server.ListenAndServe()
}

// loadKey reads the RSA signing key from ISSUER_PRIVATE_KEY_PEM (how the
// Kubernetes Secret provides it, so tokens survive restarts) or generates an
// ephemeral one for local development.
func loadKey() (jwk.Key, error) {
	var key jwk.Key
	if pem := os.Getenv("ISSUER_PRIVATE_KEY_PEM"); pem != "" {
		parsed, err := jwk.ParseKey([]byte(pem), jwk.WithPEM(true))
		if err != nil {
			return nil, err
		}
		key = parsed
	} else {
		slog.Warn("ISSUER_PRIVATE_KEY_PEM not set; generating an ephemeral key (tokens die with this process)")
		raw, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		imported, err := jwk.Import(raw)
		if err != nil {
			return nil, err
		}
		key = imported
	}
	// The key id is the RFC 7638 thumbprint, so it is stable for a given key
	// and changes when the key is rotated.
	if err := jwk.AssignKeyID(key); err != nil {
		return nil, err
	}
	if err := key.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		return nil, err
	}
	return key, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
