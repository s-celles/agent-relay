package core

// Limiter bounds concurrent backend subprocesses (REQ-PROC-02). Acquisition
// is non-blocking: a full pool is reported immediately so the caller can
// answer 503 without spawning anything (REQ-PROC-03).
type Limiter struct{ slots chan struct{} }

func NewLimiter(n int) *Limiter { return &Limiter{slots: make(chan struct{}, n)} }

func (l *Limiter) TryAcquire() bool {
	select {
	case l.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *Limiter) Release() { <-l.slots }
