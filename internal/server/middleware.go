package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/s-celles/agent-relay/internal/ratelimit"
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

// RateLimit throttles each caller independently, so one client cannot drain
// the operator's subscription. It runs after authentication (a rejected
// caller consumes no quota) and before dispatch (a throttled request spawns
// nothing). Callers are keyed by credential; with no tokens configured — a
// loopback-only posture — by remote address.
func RateLimit(l *ratelimit.Limiter, onReject func(), writeErr func(http.ResponseWriter, int, string), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := string(extractCredential(r))
		if key == "" {
			key = callerAddr(r)
		}
		ok, retryAfter := l.Allow(hashKey(key), time.Now())
		if !ok {
			if onReject != nil {
				onReject()
			}
			setRetryAfter(w, retryAfter)
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded, slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hashKey avoids keeping caller credentials as map keys in a second place.
func hashKey(k string) string {
	sum := sha256.Sum256([]byte(k))
	return string(sum[:])
}

func callerAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// setRetryAfter tells the client when to come back (RFC 9110 §10.2.3),
// rounded up to whole seconds with a one-second floor.
func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
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
