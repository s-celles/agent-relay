// Package ratelimit implements a per-caller token bucket: the relay's
// concurrency cap bounds simultaneous work, this bounds sustained rate, so
// one caller cannot drain the operator's subscription.
package ratelimit

import (
	"sync"
	"time"
)

// idleTTL reclaims buckets a caller has stopped using, so the map cannot
// grow without bound.
const idleTTL = 10 * time.Minute

type bucket struct {
	tokens   float64
	lastFill time.Time
	lastSeen time.Time
}

// Limiter allows rpm requests per minute per caller, with a burst of rpm.
// A zero rpm disables it entirely.
type Limiter struct {
	rpm     int
	mu      sync.Mutex
	buckets map[string]*bucket
}

func New(rpm int) *Limiter {
	if rpm <= 0 {
		return &Limiter{} // disabled
	}
	return &Limiter{rpm: rpm, buckets: map[string]*bucket{}}
}

// Allow consumes one token for key. When it returns false, retryAfter says
// how long until a token is available.
func (l *Limiter) Allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
	if l.rpm <= 0 {
		return true, 0
	}
	perSecond := float64(l.rpm) / 60

	l.mu.Lock()
	defer l.mu.Unlock()
	l.reclaim(now)

	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(l.rpm), lastFill: now}
		l.buckets[key] = b
	} else {
		b.tokens += now.Sub(b.lastFill).Seconds() * perSecond
		if b.tokens > float64(l.rpm) {
			b.tokens = float64(l.rpm)
		}
		b.lastFill = now
	}
	b.lastSeen = now

	if b.tokens < 1 {
		missing := 1 - b.tokens
		return false, time.Duration(missing/perSecond*float64(time.Second)) + time.Millisecond
	}
	b.tokens--
	return true, 0
}

// reclaim drops buckets untouched for idleTTL. Called under the lock.
func (l *Limiter) reclaim(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > idleTTL {
			delete(l.buckets, k)
		}
	}
}

// size reports the number of live buckets (tests).
func (l *Limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
