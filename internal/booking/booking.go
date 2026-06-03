// Package booking implements the "booking" mechanism that lets only one
// Claude Code session own the single Telegram getUpdates consumer at a time.
//
// Telegram allows exactly one consumer per bot, so if several cocote
// instances polled the same bot they would collide (HTTP 409). The lease here
// is a small lock file with a heartbeat: the holder rewrites its heartbeat
// timestamp periodically, and any instance whose heartbeat is older than the
// TTL is considered dead, allowing another session to take over.
package booking

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// instanceSeq makes lease instance ids unique even within a single process.
var instanceSeq atomic.Uint64

// Info is the on-disk record describing who currently holds the booking.
type Info struct {
	Instance    string    `json:"instance"` // unique per Lease, used to identify the owner
	PID         int       `json:"pid"`
	Session     string    `json:"session"`
	Host        string    `json:"host"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	TTLSeconds  float64   `json:"ttl_seconds"`
}

// Lease represents this process's attempt to own the Telegram consumer.
type Lease struct {
	path      string
	guardPath string
	ttl       time.Duration
	session   string
	id        string // unique owner identity stored in the lock file

	mu   sync.Mutex
	info Info
	held bool
	stop chan struct{}
}

// New creates a Lease backed by files in dir. dir is created if needed.
func New(dir, session string, ttl time.Duration) (*Lease, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("%d.%d.%d", os.Getpid(), time.Now().UnixNano(), instanceSeq.Add(1))
	return &Lease{
		path:      filepath.Join(dir, "booking.lock"),
		guardPath: filepath.Join(dir, "booking.guard"),
		ttl:       ttl,
		session:   session,
		id:        id,
		stop:      make(chan struct{}),
	}, nil
}

// TryAcquire attempts to take the booking. It returns (true, ourInfo, nil) on
// success. If another live session holds it, it returns (false, holderInfo,
// nil). On success a heartbeat goroutine is started; call Release to stop it.
func (l *Lease) TryAcquire() (bool, *Info, error) {
	if err := l.lockGuard(); err != nil {
		return false, nil, err
	}
	defer l.unlockGuard()

	now := time.Now()
	if cur, err := l.read(); err == nil && cur != nil {
		ttl := time.Duration(cur.TTLSeconds * float64(time.Second))
		expired := now.Sub(cur.HeartbeatAt) > ttl
		if !expired && cur.Instance != l.id {
			return false, cur, nil // someone else holds a live lease
		}
	}

	host, _ := os.Hostname()
	l.mu.Lock()
	l.info = Info{
		Instance:    l.id,
		PID:         os.Getpid(),
		Session:     l.session,
		Host:        host,
		AcquiredAt:  now,
		HeartbeatAt: now,
		TTLSeconds:  l.ttl.Seconds(),
	}
	info := l.info
	l.mu.Unlock()

	if err := l.write(info); err != nil {
		return false, nil, err
	}
	l.held = true
	go l.heartbeatLoop()
	return true, &info, nil
}

// Held reports whether this process currently holds the booking.
func (l *Lease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// Release stops the heartbeat and removes the lock file if we still own it.
// It is safe to call more than once.
func (l *Lease) Release() {
	l.mu.Lock()
	if !l.held {
		l.mu.Unlock()
		return
	}
	l.held = false
	close(l.stop)
	l.mu.Unlock()

	if err := l.lockGuard(); err != nil {
		return
	}
	defer l.unlockGuard()
	if cur, _ := l.read(); cur != nil && cur.Instance == l.id {
		_ = os.Remove(l.path)
	}
}

func (l *Lease) heartbeatLoop() {
	interval := l.ttl / 3
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.mu.Lock()
			l.info.HeartbeatAt = time.Now()
			info := l.info
			l.mu.Unlock()
			_ = l.write(info)
		}
	}
}

func (l *Lease) read() (*Info, error) {
	b, err := os.ReadFile(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var i Info
	if err := json.Unmarshal(b, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

func (l *Lease) write(info Info) error {
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// lockGuard serializes the read-modify-write of the lock file across
// processes using an exclusive-create guard file. A guard older than 5s is
// assumed orphaned and stolen.
func (l *Lease) lockGuard() error {
	for i := 0; i < 100; i++ {
		f, err := os.OpenFile(l.guardPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			return nil
		}
		if fi, e := os.Stat(l.guardPath); e == nil && time.Since(fi.ModTime()) > 5*time.Second {
			_ = os.Remove(l.guardPath)
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("could not acquire booking guard lock (another instance is contending)")
}

func (l *Lease) unlockGuard() { _ = os.Remove(l.guardPath) }
