package booking

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireContentionAndRelease(t *testing.T) {
	dir := t.TempDir()

	l1, err := New(dir, "s1", 10*time.Second)
	if err != nil {
		t.Fatalf("New l1: %v", err)
	}
	held, _, err := l1.TryAcquire()
	if err != nil || !held {
		t.Fatalf("l1.TryAcquire: held=%v err=%v, want held=true", held, err)
	}

	// A second session must not be able to acquire while l1 is alive.
	l2, err := New(dir, "s2", 10*time.Second)
	if err != nil {
		t.Fatalf("New l2: %v", err)
	}
	held2, holder, err := l2.TryAcquire()
	if err != nil {
		t.Fatalf("l2.TryAcquire: %v", err)
	}
	if held2 {
		t.Fatal("l2 acquired the lease while l1 still holds it")
	}
	if holder == nil || holder.Session != "s1" {
		t.Fatalf("l2 holder = %+v, want session s1", holder)
	}

	// After l1 releases, a new session can take over.
	l1.Release()
	l3, err := New(dir, "s3", 10*time.Second)
	if err != nil {
		t.Fatalf("New l3: %v", err)
	}
	held3, _, err := l3.TryAcquire()
	if err != nil || !held3 {
		t.Fatalf("l3.TryAcquire after release: held=%v err=%v, want held=true", held3, err)
	}
	l3.Release()
}

func TestTakeoverWhenLeaseIsStale(t *testing.T) {
	dir := t.TempDir()

	// Simulate a crashed session that left a lock with an old heartbeat.
	stale := Info{
		PID:         999999,
		Session:     "dead",
		Host:        "ghost",
		AcquiredAt:  time.Now().Add(-time.Hour),
		HeartbeatAt: time.Now().Add(-time.Hour),
		TTLSeconds:  30,
	}
	b, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(dir, "booking.lock"), b, 0o644); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}

	l, err := New(dir, "fresh", 30*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	held, _, err := l.TryAcquire()
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !held {
		t.Fatal("fresh session failed to take over a stale lease")
	}
	l.Release()
}
