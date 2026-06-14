package httpapi

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ── JWT test helpers ──────────────────────────────────────────────────────────

func genTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// encCoord encodes a P-256 coordinate as a 32-byte base64url string.
func encCoord(n *big.Int) string {
	b := make([]byte, 32)
	nb := n.Bytes()
	copy(b[32-len(nb):], nb)
	return base64.RawURLEncoding.EncodeToString(b)
}

func fakeJWKSServer(t *testing.T, pub *ecdsa.PublicKey, kid string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"keys": []map[string]any{{
				"kty": "EC", "use": "sig", "alg": "ES256",
				"kid": kid, "crv": "P-256",
				"x": encCoord(pub.X),
				"y": encCoord(pub.Y),
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// signToken mints an ES256 JWT with the given header typ and claims.
func signToken(t *testing.T, priv *ecdsa.PrivateKey, kid, typ string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = kid
	tok.Header["typ"] = typ
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// userToken mints a user access token (typ "JWT", act flag).
func userToken(t *testing.T, priv *ecdsa.PrivateKey, kid, issuer string, active bool, exp time.Time) string {
	return signToken(t, priv, kid, "JWT", jwt.MapClaims{
		"iss": issuer,
		"sub": "user-abc",
		"exp": exp.Unix(),
		"act": active,
	})
}

// serviceToken mints a client_credentials service token. Crucially it sets the
// JWT header typ to "at+jwt", which the verifier requires for ParseServiceToken
// (and which makes Parse reject it as a user token).
func serviceToken(t *testing.T, priv *ecdsa.PrivateKey, kid, issuer string, exp time.Time) string {
	return signToken(t, priv, kid, "at+jwt", jwt.MapClaims{
		"iss":       issuer,
		"sub":       "svc-abc",
		"exp":       exp.Unix(),
		"client_id": "greenhouse-consumer",
		"scope":     "climate.read",
	})
}

// authSetup builds a Server with IdentityURL pointing at a fake JWKS server and
// returns a handler that wraps a dummy protected endpoint with authMiddleware.
func authSetup(t *testing.T) (h http.Handler, priv *ecdsa.PrivateKey, kid, issuer string) {
	t.Helper()
	priv = genTestKey(t)
	kid = "testkey"
	fakeID := fakeJWKSServer(t, &priv.PublicKey, kid)
	s := setup(t)
	s.IdentityURL = fakeID.URL
	issuer = fakeID.URL

	dummy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h = s.authMiddleware()(dummy)
	return h, priv, kid, issuer
}

func doAuth(h http.Handler, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/series", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestAuth_NoToken_Returns401(t *testing.T) {
	h, _, _, _ := authSetup(t)
	if w := doAuth(h, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuth_ValidUserToken_Passes(t *testing.T) {
	h, priv, kid, iss := authSetup(t)
	tok := userToken(t, priv, kid, iss, true, time.Now().Add(15*time.Minute))
	if w := doAuth(h, tok); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

func TestAuth_InactiveUser_Returns401(t *testing.T) {
	h, priv, kid, iss := authSetup(t)
	tok := userToken(t, priv, kid, iss, false, time.Now().Add(15*time.Minute))
	if w := doAuth(h, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

// TestAuth_ValidServiceToken_Passes is the statehouse gap-fix: a service
// (client_credentials) token with typ "at+jwt" must be accepted.
func TestAuth_ValidServiceToken_Passes(t *testing.T) {
	h, priv, kid, iss := authSetup(t)
	tok := serviceToken(t, priv, kid, iss, time.Now().Add(15*time.Minute))
	if w := doAuth(h, tok); w.Code != http.StatusOK {
		t.Fatalf("want 200 for service token (gap fix), got %d", w.Code)
	}
}

func TestAuth_ExpiredToken_Returns401(t *testing.T) {
	h, priv, kid, iss := authSetup(t)
	tok := userToken(t, priv, kid, iss, true, time.Now().Add(-time.Minute))
	if w := doAuth(h, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuth_ExpiredServiceToken_Returns401(t *testing.T) {
	h, priv, kid, iss := authSetup(t)
	tok := serviceToken(t, priv, kid, iss, time.Now().Add(-time.Minute))
	if w := doAuth(h, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuth_GarbageToken_Returns401(t *testing.T) {
	h, _, _, _ := authSetup(t)
	if w := doAuth(h, "not-a-jwt"); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuth_WrongIssuer_Returns401(t *testing.T) {
	h, priv, kid, _ := authSetup(t)
	tok := userToken(t, priv, kid, "https://evil.example.com", true, time.Now().Add(15*time.Minute))
	if w := doAuth(h, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuth_BadSignature_Returns401(t *testing.T) {
	h, _, kid, iss := authSetup(t)
	other := genTestKey(t)
	tok := userToken(t, other, kid, iss, true, time.Now().Add(15*time.Minute))
	if w := doAuth(h, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuth_Disabled_Passthrough(t *testing.T) {
	s := setup(t) // IdentityURL == ""
	dummy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := s.authMiddleware()(dummy)
	if w := doAuth(h, ""); w.Code != http.StatusOK {
		t.Fatalf("auth disabled should pass through; want 200, got %d", w.Code)
	}
}

// TestAuth_Disabled_LogsWarning asserts that disabling auth (empty IdentityURL)
// is LOUD, never a silent accident: authMiddleware must emit a prominent warning
// so an operator who mis-configured identity.base_url sees it at startup.
func TestAuth_Disabled_LogsWarning(t *testing.T) {
	var buf bytes.Buffer
	s := setup(t) // IdentityURL == ""
	s.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	_ = s.authMiddleware()

	if !strings.Contains(buf.String(), "AUTH DISABLED") {
		t.Errorf("expected an AUTH DISABLED warning when identity.base_url is empty; got %q", buf.String())
	}
}

// TestHealth_ReportsAuthStatus surfaces the auth posture on /healthz so
// monitoring can alert when the data API is running unauthenticated.
func TestHealth_ReportsAuthStatus(t *testing.T) {
	authOf := func(s *Server) string {
		w := httptest.NewRecorder()
		s.handleHealth(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var h struct {
			Auth string `json:"auth"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
			t.Fatalf("decode health: %v", err)
		}
		return h.Auth
	}

	if got := authOf(setup(t)); got != "disabled" {
		t.Errorf("auth status with empty IdentityURL = %q, want disabled", got)
	}

	s := setup(t)
	s.IdentityURL = "https://id.example"
	if got := authOf(s); got != "enabled" {
		t.Errorf("auth status with IdentityURL set = %q, want enabled", got)
	}
}
