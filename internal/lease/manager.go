package lease

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
	records map[string]Record
	stopper Stopper
	stop    chan struct{}
}

func NewManager(stopper Stopper) *Manager {
	manager := &Manager{
		records: map[string]Record{},
		stopper: stopper,
		stop:    make(chan struct{}),
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
	record := Record{
		UnitID: unitID, Token: token, TTL: ttl,
		ExpiresAt: now.Add(ttl),
	}
	m.records[unitID] = record
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
}

func (m *Manager) watch() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-ticker.C:
			var expired []Record
			m.mu.Lock()
			for unitID, record := range m.records {
				if !now.Before(record.ExpiresAt) {
					expired = append(expired, record)
					delete(m.records, unitID)
				}
			}
			m.mu.Unlock()

			if m.stopper != nil {
				for _, record := range expired {
					_ = m.stopper.StopLeaseUnit(record.UnitID)
				}
			}
		}
	}
}
