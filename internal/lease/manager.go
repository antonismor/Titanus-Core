package lease

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Stopper interface {
	StopLeaseUnit(id string) error
}

type Record struct {
	UnitID    string        `json:"unit_id"`
	Token     string        `json:"token"`
	TTL       time.Duration `json:"ttl"`
	ExpiresAt time.Time     `json:"expires_at"`
}

type Manager struct {
	mu      sync.Mutex
	path    string
	bootID  string
	done    chan struct{}
	records map[string]Record
	stopper Stopper
	stop    chan struct{}
}

func NewManager(stopper Stopper) *Manager {
	manager := &Manager{
		records: map[string]Record{},
		stopper: stopper,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go manager.watch()
	return manager
}

func NewToken() (string, error) {
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func (m *Manager) Renew(unitID, token string, ttl time.Duration) (Record, error) {
	if unitID == "" || token == "" {
		return Record{}, fmt.Errorf("Unit ID and lease token are required")
	}
	if ttl < 5*time.Second || ttl > 5*time.Minute {
		return Record{}, fmt.Errorf("lease TTL must be between 5s and 5m")
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	current, exists := m.records[unitID]
	if exists && current.Token != token && now.Before(current.ExpiresAt) {
		return Record{}, fmt.Errorf("Unit %s already has a different live lease", unitID)
	}
	if exists && !now.Before(current.ExpiresAt) && m.stopper != nil {
		if err := m.stopper.StopLeaseUnit(unitID); err != nil {
			return Record{}, fmt.Errorf("fence expired Unit before renewing: %w", err)
		}
	}
	record := Record{
		UnitID: unitID, Token: token, TTL: ttl,
		ExpiresAt: now.Add(ttl),
	}
	m.records[unitID] = record
	if err := m.persistLocked(); err != nil {
		if exists {
			m.records[unitID] = current
		} else {
			delete(m.records, unitID)
		}
		return Record{}, err
	}
	return record, nil
}

func (m *Manager) Revoke(unitID, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, exists := m.records[unitID]
	if !exists {
		return nil
	}
	if token != "" && current.Token != token {
		return fmt.Errorf("lease token mismatch for Unit %s", unitID)
	}
	delete(m.records, unitID)
	if err := m.persistLocked(); err != nil {
		m.records[unitID] = current
		return err
	}
	return nil
}

func (m *Manager) Inspect(unitID string) (Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[unitID]
	return record, ok
}

func (m *Manager) Close() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
}

func (m *Manager) watch() {
	defer close(m.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-ticker.C:
			m.expire(now)
		}
	}
}

// Expiry and renewal share the same lock: a new lease cannot race with an
// expired lease's stop operation. Stop failures remain recorded and retry.
func (m *Manager) expire(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, record := range m.records {
		if now.Before(record.ExpiresAt) {
			continue
		}
		if m.stopper != nil {
			if err := m.stopper.StopLeaseUnit(id); err != nil {
				continue
			}
		}
		delete(m.records, id)
		if err := m.persistLocked(); err != nil {
			m.records[id] = record
		}
	}
}

type persisted struct {
	BootID  string            `json:"boot_id"`
	Records map[string]Record `json:"records"`
}

// Open restores the original deadlines. A reboot invalidates all prior leases;
// expired Units are stopped before this function returns to expose the API.
func Open(stateRoot string, stopper Stopper) (*Manager, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(stateRoot, "leases.json")
	manager := &Manager{records: map[string]Record{}, stopper: stopper, stop: make(chan struct{}), done: make(chan struct{}), path: path, bootID: string(boot)}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		var saved persisted
		if err := json.Unmarshal(data, &saved); err != nil {
			return nil, fmt.Errorf("restore leases: %w", err)
		}
		if saved.Records != nil {
			manager.records = saved.Records
		}
		if saved.BootID != manager.bootID {
			for id, record := range manager.records {
				record.ExpiresAt = time.Time{}
				manager.records[id] = record
			}
		}
	}
	if err := os.MkdirAll(stateRoot, 0750); err != nil {
		return nil, err
	}
	manager.expire(time.Now())
	if err := manager.persistLocked(); err != nil {
		return nil, err
	}
	go manager.watch()
	return manager, nil
}

func (m *Manager) persistLocked() error {
	if m.path == "" {
		return nil
	}
	return durable.WriteJSON(m.path, persisted{BootID: m.bootID, Records: m.records}, 0600)
}
