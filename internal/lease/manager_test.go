package lease

import (
	"sync"
	"testing"
	"time"
)

type fakeStopper struct {
	mu      sync.Mutex
	stopped []string
}

func (f *fakeStopper) StopLeaseUnit(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}

func TestLeaseRenewRejectsConcurrentToken(t *testing.T) {
	manager := NewManager(nil)
	defer manager.Close()
	if _, err := manager.Renew("db01", "a", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Renew("db01", "b", 5*time.Second); err == nil {
		t.Fatal("expected live lease conflict")
	}
}

func TestLeaseTokenGeneration(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || a == b {
		t.Fatal("lease tokens must be non-empty and unique")
	}
}
