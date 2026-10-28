package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/sweeney/identity/common/auth"
)

// authMiddleware returns a middleware that validates Bearer JWTs when
// IdentityURL is set. When IdentityURL is empty it returns a no-op wrapper so
// existing tests and local runs work without auth configured.
func (s *Server) authMiddleware() func(http.Handler) http.Handler {
	if s.IdentityURL == "" {
		if s.Logger != nil {
			s.Logger.Warn("AUTH DISABLED: identity.base_url is empty — all data routes are PUBLIC; " +
				"set identity.base_url, or auth.allow_insecure to acknowledge running without auth")
		}
		return func(h http.Handler) http.Handler { return h }
	}
	verifier, err := auth.NewJWKSVerifier(auth.JWKSVerifierConfig{
		IssuerURL: s.IdentityURL,
		Issuer:    s.IdentityURL,
		Logger:    s.Logger,
	})
	if err != nil {
		// Only fails when IssuerURL or Issuer is empty, guarded above.
		panic(err)
	}
	s.verifier = verifier
	return func(h http.Handler) http.Handler { return requireAuth(verifier, h) }
}

// requireAuth rejects requests lacking a valid Bearer JWT. Greenhouse accepts
// BOTH (mirroring countinghouse's statehouse gap-fix):
//   - a user token (Parse succeeds and the user is active), OR
//   - a service token (ParseServiceToken succeeds — a successful parse of a
//     client_credentials token is itself the validity signal; there is no
//     IsActive flag on service tokens).
//
// Greenhouse is likely called by other services, so service tokens must work.
//
// A token that could not be CHECKED is answered 503, not 401. A 401 tells the
// client to discard its credentials, so answering one during an identity outage
// would sign every user out at the same moment, over tokens nobody examined.
// identity/common reports that case as ErrKeysUnavailable: the JWKS endpoint is
// unreachable and the cached keys are absent or older than its MaxStaleAge.
func requireAuth(verifier *auth.JWKSVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reject := func() {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+verifier.Issuer()+`"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			reject()
			return
		}
		// User token: valid and active.
		c, userErr := verifier.Parse(r.Context(), token)
		if userErr == nil && c.IsActive {
			next.ServeHTTP(w, r)
			return
		}
		// Service token: a successful parse is the validity signal.
		_, svcErr := verifier.ParseServiceToken(r.Context(), token)
		if svcErr == nil {
			next.ServeHTTP(w, r)
			return
		}
		// Either parser can be the one that reached the keys: each refuses the
		// other's token kind before looking at its signature.
		if errors.Is(userErr, auth.ErrKeysUnavailable) || errors.Is(svcErr, auth.ErrKeysUnavailable) {
			unavailable(w)
			return
		}
		reject()
	})
}

// keysUnavailableRetryAfter is the Retry-After on a 503 for unverifiable
// tokens. The verifier rate-limits its own refetches, so a client retrying
// sooner than this would mostly be answered from the same failed state.
const keysUnavailableRetryAfter = "30"

// unavailable answers a request whose token could not be checked at all.
//
// No WWW-Authenticate: the client's credentials are not in question, and a
// challenge invites it to go and get new ones. Not logged here either, because
// the verifier already logs each failed fetch, and a line per request would
// multiply one outage by the request rate.
func unavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", keysUnavailableRetryAfter)
	writeError(w, http.StatusServiceUnavailable, "authentication temporarily unavailable")
}
