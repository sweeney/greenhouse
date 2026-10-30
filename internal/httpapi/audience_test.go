package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sweeney/greenhouse/internal/config"
)

// audienceSetup is authSetup with an audience mode and a captured log.
func audienceSetup(t *testing.T, mode string) (h http.Handler, logs *bytes.Buffer, mint func(typ string, aud any) string) {
	t.Helper()
	priv := genTestKey(t)
	kid := "testkey"
	fakeID := fakeJWKSServer(t, &priv.PublicKey, kid)
	s := setup(t)
	s.IdentityURL = fakeID.URL
	s.AudienceCheck = mode
	logs = &bytes.Buffer{}
	s.Logger = slog.New(slog.NewTextHandler(logs, nil))
	h = s.authMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mint = func(typ string, aud any) string {
		c := jwt.MapClaims{"iss": fakeID.URL, "exp": time.Now().Add(15 * time.Minute).Unix()}
		if typ == "at+jwt" {
			c["sub"], c["client_id"], c["scope"] = "svc-abc", "some-service", "climate.read"
		} else {
			c["sub"], c["act"] = "user-abc", true
		}
		if aud != nil {
			c["aud"] = aud
		}
		return signToken(t, priv, kid, typ, c)
	}
	return h, logs, mint
}

const audienceWarning = "token would be rejected by audience enforcement"

// A wrong audience and a missing one are the same mistake, so warn reports
// both, as enforce refuses both. A client_credentials token minted without
// any aud is the likeliest real case.
func TestAudienceWarnAcceptsAWrongOrMissingAudienceAndLogsIt(t *testing.T) {
	for _, typ := range []string{"JWT", "at+jwt"} {
		for _, aud := range []any{nil, []string{"mqttauth"}} {
			h, logs, mint := audienceSetup(t, config.AudienceWarn)
			if w := doAuth(h, mint(typ, aud)); w.Code != http.StatusOK {
				t.Errorf("%s aud=%v: want 200 in warn mode, got %d", typ, aud, w.Code)
			}
			if !strings.Contains(logs.String(), audienceWarning) || !strings.Contains(logs.String(), Audience) {
				t.Errorf("%s aud=%v: want a warning naming %q, log was:\n%s", typ, aud, Audience, logs)
			}
		}
	}
}

func TestAudienceWarnIsQuietForTheRightAudience(t *testing.T) {
	// The aud a real user token carries today.
	user := []string{"config", "countinghouse", Audience, "id.swee.net", "mqttauth", "statehouse"}
	for _, typ := range []string{"JWT", "at+jwt"} {
		h, logs, mint := audienceSetup(t, config.AudienceWarn)
		if w := doAuth(h, mint(typ, user)); w.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", typ, w.Code)
		}
		if strings.Contains(logs.String(), audienceWarning) {
			t.Errorf("%s: no warning wanted for a token naming %s, log was:\n%s", typ, Audience, logs)
		}
	}
}

func TestAudienceEnforceRefusesAWrongOrMissingAudience(t *testing.T) {
	for _, typ := range []string{"JWT", "at+jwt"} {
		for _, aud := range []any{nil, []string{"mqttauth"}} {
			h, _, mint := audienceSetup(t, config.AudienceEnforce)
			if w := doAuth(h, mint(typ, aud)); w.Code != http.StatusUnauthorized {
				t.Errorf("%s aud=%v: want 401 when enforcing, got %d", typ, aud, w.Code)
			}
		}
	}
}

func TestAudienceEnforceAcceptsTheRightAudience(t *testing.T) {
	for _, typ := range []string{"JWT", "at+jwt"} {
		h, _, mint := audienceSetup(t, config.AudienceEnforce)
		if w := doAuth(h, mint(typ, []string{Audience})); w.Code != http.StatusOK {
			t.Errorf("%s: want 200, got %d", typ, w.Code)
		}
	}
}

func TestAudienceOffNeitherRefusesNorLogs(t *testing.T) {
	h, logs, mint := audienceSetup(t, config.AudienceOff)
	if w := doAuth(h, mint("JWT", []string{"mqttauth"})); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if strings.Contains(logs.String(), audienceWarning) {
		t.Errorf("off must not log, log was:\n%s", logs)
	}
}

// A mode that reaches the Server without going through Load must not quietly
// mean off either.
func TestAudienceUnknownModePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("want a panic for an unknown audience mode")
		}
	}()
	audienceSetup(t, "enforcing")
}

// The spec documents a 401 as an application/json ErrorResponse; it used to be
// http.Error's text/plain.
func TestUnauthorizedIsAJSONErrorResponse(t *testing.T) {
	h, _, mint := audienceSetup(t, config.AudienceEnforce)
	for name, tok := range map[string]string{
		"no token":       "",
		"wrong audience": mint("JWT", []string{"mqttauth"}),
	} {
		w := doAuth(h, tok)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: want 401, got %d", name, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: Content-Type = %q, want application/json", name, ct)
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error == "" {
			t.Errorf("%s: body %q is not an ErrorResponse: %v", name, w.Body.String(), err)
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: a 401 must keep its WWW-Authenticate challenge", name)
		}
	}
}
