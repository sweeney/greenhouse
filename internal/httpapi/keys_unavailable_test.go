package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// When the identity service cannot be reached, a token cannot be checked at all.
// That is an outage, not a verdict on the token: answering 401 tells the client
// to sign its user out, so an identity outage would sign every user out of
// every service at once, over tokens nobody examined. identity/common reports
// it as ErrKeysUnavailable (no keys yet, or keys older than MaxStaleAge); it
// must be a 503, and a genuinely bad token must still be a 401 (auth_test.go).

// unavailableAuthSetup wraps a dummy endpoint with authMiddleware against an
// identity service that answers every JWKS request with a 500, so the verifier
// never holds a key.
func unavailableAuthSetup(t *testing.T) (h http.Handler, issuer string) {
	t.Helper()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "identity is down", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	s := setup(t)
	s.IdentityURL = down.URL
	dummy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return s.authMiddleware()(dummy), down.URL
}

func TestAuth_KeysUnavailable_UserToken_Returns503(t *testing.T) {
	h, issuer := unavailableAuthSetup(t)
	priv := genTestKey(t)
	w := doAuth(h, userToken(t, priv, "testkey", issuer, true, time.Now().Add(time.Hour)))
	assertKeysUnavailable(t, w)
}

func TestAuth_KeysUnavailable_ServiceToken_Returns503(t *testing.T) {
	h, issuer := unavailableAuthSetup(t)
	priv := genTestKey(t)
	w := doAuth(h, serviceToken(t, priv, "testkey", issuer, time.Now().Add(time.Hour)))
	assertKeysUnavailable(t, w)
}

// Without a token there is nothing to check, so an identity outage changes
// nothing: it is still a 401.
func TestAuth_KeysUnavailable_NoToken_StillReturns401(t *testing.T) {
	h, _ := unavailableAuthSetup(t)
	if w := doAuth(h, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func assertKeysUnavailable(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d (body %q)", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("503 carries no Retry-After")
	}
	// No challenge: the client's credentials are not in question, and a
	// WWW-Authenticate header invites it to go and get new ones.
	if h := w.Header().Get("WWW-Authenticate"); h != "" {
		t.Errorf("503 carries a WWW-Authenticate challenge: %q", h)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Errorf("503 body is not the JSON error envelope: %q", w.Body.String())
	}
}

// publicPaths are the routes newMux registers without authMiddleware.
var publicPaths = map[string]bool{"/healthz": true, "/openapi.json": true}

// Every data route sits behind the auth middleware, so every one can answer 401
// and 503, and a client generated off the spec should know both before it meets
// them. spec_test.go already holds the spec's paths equal to newMux's routes.
func TestOpenAPISpec_AuthenticatedRoutesDocument401And503(t *testing.T) {
	s := setup(t)
	w := httptest.NewRecorder()
	newMux(s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	checked := 0
	for path, ops := range doc.Paths {
		if publicPaths[path] {
			continue
		}
		for method, op := range ops {
			checked++
			for _, code := range []string{"401", "503"} {
				if _, ok := op.Responses[code]; !ok {
					t.Errorf("%s %s does not document %s", method, path, code)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("spec has no authenticated operations; is /openapi.json being served?")
	}
}
