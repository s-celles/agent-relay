package ratelimit

import (
	"testing"
	"time"
)

func TestDisabledLimiterAllowsEverything(t *testing.T) {
	l := New(0)
	now := time.Now()
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow("k", now); !ok {
			t.Fatal("a disabled limiter must allow every request")
		}
	}
}

func TestBurstThenRefusal(t *testing.T) {
	l := New(60) // 60/min → one token per second, burst 60
	now := time.Now()

	for i := 0; i < 60; i++ {
		if ok, _ := l.Allow("caller", now); !ok {
			t.Fatalf("request %d of the burst was refused", i)
		}
	}
	ok, retry := l.Allow("caller", now)
	if ok {
		t.Fatal("the 61st request in the same instant must be refused")
	}
	if retry <= 0 || retry > time.Second+10*time.Millisecond {
		t.Errorf("retryAfter = %v, want ~1s", retry)
	}
}

func TestRefillOverTime(t *testing.T) {
	l := New(60)
	now := time.Now()
	for i := 0; i < 60; i++ {
		l.Allow("caller", now)
	}
	if ok, _ := l.Allow("caller", now); ok {
		t.Fatal("bucket should be empty")
	}
	// One second later, exactly one token is back.
	if ok, _ := l.Allow("caller", now.Add(time.Second)); !ok {
		t.Fatal("a token should have refilled after one second")
	}
	if ok, _ := l.Allow("caller", now.Add(time.Second)); ok {
		t.Fatal("only one token should have refilled")
	}
}

func TestPerCallerIsolation(t *testing.T) {
	l := New(2)
	now := time.Now()
	l.Allow("alice", now)
	l.Allow("alice", now)
	if ok, _ := l.Allow("alice", now); ok {
		t.Fatal("alice should be out of quota")
	}
	if ok, _ := l.Allow("bob", now); !ok {
		t.Fatal("bob must have his own bucket")
	}
}

func TestIdleBucketsAreReclaimed(t *testing.T) {
	l := New(60)
	now := time.Now()
	for i := 0; i < 200; i++ {
		l.Allow(string(rune('a'+i%26))+string(rune('a'+i/26)), now)
	}
	// Long after their last use, idle buckets must not accumulate forever.
	l.Allow("fresh", now.Add(time.Hour))
	if n := l.size(); n > 10 {
		t.Errorf("bucket count = %d after a long idle gap, want the map reclaimed", n)
	}
}
