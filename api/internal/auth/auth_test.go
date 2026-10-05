package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/ArianKhademi/rigforge/api/internal/apitest"
	"github.com/ArianKhademi/rigforge/api/internal/auth/authtest"
)

// request sends GET /api/me with the given raw Authorization header.
func request(e *apitest.Env, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	e.Server.Router.ServeHTTP(rec, req)
	return rec
}

func TestValidTokenIsAccepted(t *testing.T) {
	e := apitest.New(t)
	rec := request(e, "Bearer "+e.Issuer.Token(t, "alice", time.Hour))
	apitest.WantStatus(t, rec, http.StatusOK)
	if got := apitest.Decode[map[string]string](t, rec)["userId"]; got != "alice" {
		t.Fatalf("userId = %q, want alice (the sub claim)", got)
	}
}

func TestRejectedTokens(t *testing.T) {
	e := apitest.New(t)

	// A key the api has never heard of, reusing the trusted key id.
	strangerKey := authtest.NewKey(t, "test-key")

	// HS256 token "signed" with a shared secret: the algorithm-confusion attack.
	hsToken, err := jwt.NewBuilder().Issuer(authtest.IssuerName).Audience([]string{authtest.Audience}).
		Subject("mallory").Expiration(time.Now().Add(time.Hour)).Build()
	if err != nil {
		t.Fatal(err)
	}
	hsSigned, err := jwt.Sign(hsToken, jwt.WithKey(jwa.HS256(), []byte("not-a-real-secret-not-a-real-secret")))
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"missing header":  "",
		"not bearer":      "Basic YWxpY2U6cGFzcw==",
		"empty bearer":    "Bearer ",
		"garbage":         "Bearer not.a.jwt",
		"expired":         "Bearer " + e.Issuer.Token(t, "alice", -time.Hour),
		"wrong signature": "Bearer " + authtest.Sign(t, strangerKey, authtest.IssuerName, authtest.Audience, "alice", time.Hour),
		"wrong algorithm": "Bearer " + string(hsSigned),
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			rec := request(e, header)
			apitest.WantStatus(t, rec, http.StatusUnauthorized)
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without a WWW-Authenticate header")
			}
		})
	}
}

func TestWrongIssuerAndAudienceAreRejected(t *testing.T) {
	e := apitest.New(t)
	// Signed by the trusted key, so only the claim check can reject these.
	for name, tok := range map[string]string{
		"wrong issuer":   e.Issuer.TokenWith(t, "someone-else", authtest.Audience, "alice", time.Hour),
		"wrong audience": e.Issuer.TokenWith(t, authtest.IssuerName, "another-api", "alice", time.Hour),
		"empty subject":  e.Issuer.TokenWith(t, authtest.IssuerName, authtest.Audience, "", time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			apitest.WantStatus(t, request(e, "Bearer "+tok), http.StatusUnauthorized)
		})
	}
}

func TestEveryAPIRouteRequiresAToken(t *testing.T) {
	e := apitest.New(t)
	for _, route := range e.Server.Router.Routes() {
		if len(route.Path) < 5 || route.Path[:5] != "/api/" || route.Path == "/api/health" {
			continue
		}
		req := httptest.NewRequest(route.Method, route.Path, nil)
		rec := httptest.NewRecorder()
		e.Server.Router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", route.Method, route.Path, rec.Code)
		}
	}
}

func TestHealthIsPublic(t *testing.T) {
	e := apitest.New(t)
	for _, path := range []string{"/healthz", "/readyz", "/api/health"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		e.Server.Router.ServeHTTP(rec, req)
		apitest.WantStatus(t, rec, http.StatusOK)
	}
}
