package core

import "testing"

func TestLimiterAcquireRelease(t *testing.T) {
	l := NewLimiter(2)

	if !l.TryAcquire() {
		t.Fatal("first acquire should succeed")
	}
	if !l.TryAcquire() {
		t.Fatal("second acquire should succeed")
	}
	if l.TryAcquire() {
		t.Fatal("third acquire should fail: pool is full")
	}

	l.Release()
	if !l.TryAcquire() {
		t.Fatal("acquire after release should succeed")
	}
}

func TestLimiterNeverBlocks(t *testing.T) {
	l := NewLimiter(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.TryAcquire()
		l.TryAcquire() // must return immediately even when full
	}()
	<-done
}
