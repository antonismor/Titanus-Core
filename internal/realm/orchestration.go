package realm

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"time"
)

type TaskPhase string

const (
	TaskPending    TaskPhase = "PENDING"
	TaskPrepared   TaskPhase = "PREPARED"
	TaskDispatched TaskPhase = "DISPATCHED"
	TaskRunning    TaskPhase = "RUNNING"
	TaskUnknown    TaskPhase = "UNKNOWN"
	TaskSucceeded  TaskPhase = "SUCCEEDED"
	TaskFailed     TaskPhase = "FAILED"
	TaskCancelled  TaskPhase = "CANCELLED"
)

type Task struct {
	Name            string            `json:"name"`
	Template        UnitTemplate      `json:"template"`
	RequiredLabels  map[string]string `json:"required_labels,omitempty"`
	Phase           TaskPhase         `json:"phase"`
	NodeID          string            `json:"node_id,omitempty"`
	UnitID          string            `json:"unit_id"`
	LeaseToken      string            `json:"lease_token,omitempty"`
	RunID           string            `json:"run_id,omitempty"`
	ExitCode        *int              `json:"exit_code,omitempty"`
	CancelRequested bool              `json:"cancel_requested"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

func (t Task) Terminal() bool {
	return t.Phase == TaskSucceeded || t.Phase == TaskFailed || t.Phase == TaskCancelled
}
func (s *Store) CreateTask(t Task) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	if len(s.data.Tasks) >= 128 {
		return fmt.Errorf("retained Task limit reached")
	}
	if !secrets.Name.MatchString(t.Name) {
		return fmt.Errorf("invalid Task name")
	}
	if _, ok := s.data.Tasks[t.Name]; ok {
		return fmt.Errorf("Task name already exists; execution is immutable")
	}
	t.UnitID = "task-" + t.Name
	t.Template.Health.Normalize("never")
	if t.Template.Health.Restart != "never" {
		return fmt.Errorf("Tasks require restart never")
	}
	if len(t.Template.Secrets) > 0 {
		if e := s.validateSecretsLocked(t.Template); e != nil {
			return e
		}
	}
	spec := unitruntime.Spec{ID: t.UnitID, Source: t.Template.Source, Command: t.Template.Command, Environment: t.Template.Environment, MemoryBytes: t.Template.MemoryBytes, CPUPercent: t.Template.CPUPercent, PidsMax: t.Template.PidsMax, Security: t.Template.Security, Health: t.Template.Health, Network: unitruntime.NetworkSpec{Fabric: t.Template.Fabric, Ports: t.Template.Ports}, Mounts: t.Template.Mounts}
	spec.Normalize()
	if e := spec.Validate(); e != nil {
		return e
	}
	// Normalize all launch fields so retries cannot produce a different spec.
	t.Template.MemoryBytes = spec.MemoryBytes
	t.Template.CPUPercent = spec.CPUPercent
	t.Template.PidsMax = spec.PidsMax
	t.Template.Security = spec.Security
	t.Template.Health = spec.Health
	t.Phase = TaskPending
	t.NodeID = ""
	t.LeaseToken = ""
	t.RunID = ""
	t.ExitCode = nil
	t.CancelRequested = false
	t.UpdatedAt = time.Now().UTC()
	if s.data.Tasks == nil {
		s.data.Tasks = map[string]Task{}
	}
	s.data.Tasks[t.Name] = t
	return s.commitLocked()
}

// UpdateTask is internal reconciliation state, never an API accepting client status.
func (s *Store) UpdateTask(t Task) error {
	s.lock()
	defer s.unlock()
	old, ok := s.data.Tasks[t.Name]
	if !ok || old.Terminal() {
		return fmt.Errorf("Task cannot be updated")
	}
	t.Template = old.Template
	t.RequiredLabels = old.RequiredLabels
	t.UnitID = old.UnitID
	t.CancelRequested = old.CancelRequested
	t.UpdatedAt = time.Now().UTC()
	s.data.Tasks[t.Name] = t
	return s.commitLocked()
}
func (s *Store) CancelTask(name string) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	t, ok := s.data.Tasks[name]
	if !ok {
		return fmt.Errorf("unknown Task")
	}
	if t.Terminal() {
		return fmt.Errorf("Task is terminal")
	}
	t.CancelRequested = true
	t.UpdatedAt = time.Now().UTC()
	s.data.Tasks[name] = t
	return s.commitLocked()
}
func (s *Store) PutSecret(r secrets.Record) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	if r.Realm != s.data.Name || !secrets.Name.MatchString(r.Name) || r.Version != uint64(len(s.data.Secrets[r.Name])+1) || len(r.Ciphertext) == 0 {
		return fmt.Errorf("invalid secret version")
	}
	if len(s.data.Secrets[r.Name]) >= 32 {
		return fmt.Errorf("secret version limit reached")
	}
	if s.data.Secrets == nil {
		s.data.Secrets = map[string][]secrets.Record{}
	}
	s.data.Secrets[r.Name] = append(s.data.Secrets[r.Name], r)
	return s.commitLocked()
}
func (s *Store) validateSecretsLocked(t UnitTemplate) error {
	if e := secrets.ValidateRefs(t.Secrets, t.Environment); e != nil {
		return e
	}
	for _, r := range t.Secrets {
		found := false
		for _, v := range s.data.Secrets[r.Name] {
			if v.Version == r.Version && len(v.Ciphertext) > 0 {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("secret version unavailable")
		}
	}
	return nil
}
func BindSecrets(state State, t UnitTemplate, spec *unitruntime.Spec) error {
	if len(t.Secrets) == 0 {
		return nil
	}
	spec.SecretRealm = state.Name
	spec.Secrets = append([]secrets.Ref(nil), t.Secrets...)
	for _, r := range t.Secrets {
		found := false
		for _, v := range state.Secrets[r.Name] {
			if v.Version == r.Version && len(v.Ciphertext) > 0 {
				spec.SecretBindings = append(spec.SecretBindings, secrets.Binding{Ref: r, Record: v})
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("secret version unavailable")
		}
	}
	return nil
}
func (s *Store) DeleteSecret(name string) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	uses := func(t UnitTemplate) bool {
		for _, r := range t.Secrets {
			if r.Name == name {
				return true
			}
		}
		return false
	}
	for _, f := range s.data.Fleets {
		if uses(f.Template) {
			return fmt.Errorf("secret referenced by Fleet")
		}
		for _, h := range f.History {
			if uses(h.Template) {
				return fmt.Errorf("secret referenced by rollback history")
			}
		}
	}
	for _, t := range s.data.Tasks {
		if uses(t.Template) {
			return fmt.Errorf("secret referenced by retained Task")
		}
	}
	if _, ok := s.data.Secrets[name]; !ok {
		return fmt.Errorf("unknown secret")
	}
	for i := range s.data.Secrets[name] {
		s.data.Secrets[name][i].Ciphertext = nil
		s.data.Secrets[name][i].Nonce = nil
	}
	return s.commitLocked()
}

type Autoscaler struct {
	Fleet            string    `json:"fleet"`
	Min              int       `json:"min"`
	Max              int       `json:"max"`
	TargetCPU        int       `json:"target_cpu"`
	CooldownSeconds  int       `json:"cooldown_seconds"`
	DownscaleSeconds int       `json:"downscale_seconds"`
	LastScaled       time.Time `json:"last_scaled,omitempty"`
	LowSince         time.Time `json:"low_since,omitempty"`
}

func (s *Store) PutAutoscaler(a Autoscaler) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	f, ok := s.data.Fleets[a.Fleet]
	if !ok {
		return fmt.Errorf("unknown Fleet")
	}
	if a.Min < 1 || a.Max < a.Min || a.Max > 1000 || a.TargetCPU < 10 || a.TargetCPU > 90 || a.CooldownSeconds < 10 || a.CooldownSeconds > 3600 || a.DownscaleSeconds < 30 || a.DownscaleSeconds > 86400 || f.Template.CPUPercent < 1 || len(f.Template.Mounts) > 0 {
		return fmt.Errorf("invalid autoscaling policy; requires explicit CPU limit and disk-free Fleet")
	}
	if a.Min < f.MinimumAvailable {
		return fmt.Errorf("autoscaler minimum must respect minimum_available")
	}
	a.LastScaled = time.Now().UTC()
	a.LowSince = time.Time{}
	if s.data.Autoscalers == nil {
		s.data.Autoscalers = map[string]Autoscaler{}
	}
	s.data.Autoscalers[a.Fleet] = a
	return s.commitLocked()
}
func (s *Store) DeleteAutoscaler(name string) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	delete(s.data.Autoscalers, name)
	return s.commitLocked()
}

// ApplyAutoscale requires the orchestration lock, held by the reconciler. A
// persistent low-load window and cooldown survive leader and daemon changes.
func (s *Store) ApplyAutoscale(name string, desired int, now time.Time) error {
	s.lock()
	defer s.unlock()
	a, ok := s.data.Autoscalers[name]
	if !ok {
		return nil
	}
	f, ok := s.data.Fleets[name]
	if !ok {
		return nil
	}
	if desired < a.Min {
		desired = a.Min
	}
	if desired > a.Max {
		desired = a.Max
	}
	if desired < f.Instances {
		if a.LowSince.IsZero() {
			a.LowSince = now
			s.data.Autoscalers[name] = a
			return s.commitLocked()
		}
		if now.Sub(a.LowSince) < time.Duration(a.DownscaleSeconds)*time.Second {
			return nil
		}
	} else if !a.LowSince.IsZero() {
		a.LowSince = time.Time{}
		s.data.Autoscalers[name] = a
		if e := s.commitLocked(); e != nil {
			return e
		}
	}
	if desired == f.Instances || now.Sub(a.LastScaled) < time.Duration(a.CooldownSeconds)*time.Second {
		return nil
	}
	if desired < f.MinimumAvailable {
		return nil
	}
	f.Instances = desired
	f.UpdatedAt = now
	a.LastScaled = now
	a.LowSince = time.Time{}
	s.data.Fleets[name] = f
	s.data.Autoscalers[name] = a
	return s.commitLocked()
}
func (s *Store) ResetAutoscaleWindow(name string) error {
	s.lock()
	defer s.unlock()
	a, ok := s.data.Autoscalers[name]
	if !ok || a.LowSince.IsZero() {
		return nil
	}
	a.LowSince = time.Time{}
	s.data.Autoscalers[name] = a
	return s.commitLocked()
}
