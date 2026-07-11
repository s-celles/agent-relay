package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireBearer rejects requests whose credential matches no configured
// token, using constant-time comparison (REQ-AUTH-04). Rejection happens
// before any dispatch: an unauthenticated caller never causes a subprocess
// spawn (REQ-AUTH-02). With no tokens configured — which Config.Validate
// only permits on loopback binds — all callers pass.
func RequireBearer(tokens [][]byte, onReject func(), writeErr func(http.ResponseWriter, int, string), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(tokens) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		got := extractCredential(r)
		for _, t := range tokens {
			if subtle.ConstantTimeCompare(got, t) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		if onReject != nil {
			onReject()
		}
		writeErr(w, http.StatusUnauthorized, "unauthorized")
	})
}

// extractCredential accepts either `Authorization: Bearer <token>` or the
// Anthropic-style `x-api-key: <token>` header.
func extractCredential(r *http.Request) []byte {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if tok, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return []byte(tok)
		}
		return nil // malformed scheme never matches
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		return []byte(key)
	}
	return nil
}
