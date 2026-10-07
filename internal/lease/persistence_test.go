package lease

import (
	"fmt"
	"testing"
	"time"
)

type failedStopper struct {
	fail  bool
	calls int
}

func (s *failedStopper) StopLeaseUnit(string) error {
	s.calls++
	if s.fail {
		return fmt.Errorf("temporary stop failure")
	}
	return nil
}

func TestLeaseSurvivesRestartWithoutExtendingDeadline(t *testing.T) {
	root := t.TempDir()
	m, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := m.Renew("unit", "token", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	m, err = Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	restored, ok := m.Inspect("unit")
	if !ok || !restored.ExpiresAt.Equal(record.ExpiresAt) || restored.Token != record.Token {
		t.Fatalf("lease lost or extended: %+v", restored)
	}
	if _, err := m.Renew("unit", "conflicting", 5*time.Second); err == nil {
		t.Fatal("restart allowed conflicting lease")
	}
	if err := m.Revoke("unit", "token"); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredLeaseRetriesStopAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	stopper := &failedStopper{fail: true}
	m, err := Open(root, stopper)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.records["unit"] = Record{UnitID: "unit", Token: "old", ExpiresAt: time.Now().Add(-time.Second)}
	err = m.persistLocked()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	m.expire(time.Now())
	m.Close()
	if stopper.calls != 1 {
		t.Fatal("expiry did not attempt stop")
	}
	stopper.fail = false
	m, err = Open(root, stopper)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, ok := m.Inspect("unit"); ok || stopper.calls != 2 {
		t.Fatal("expired lease was forgotten instead of retried on recovery")
	}
}
