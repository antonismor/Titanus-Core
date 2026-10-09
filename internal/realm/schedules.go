package realm

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"reflect"
	"time"
)

// Every attempt is a distinct immutable ordinary Task. A schedule never resets
// a dispatched/UNKNOWN Task or reuses its Unit, execution identity or lease.
type TaskSchedule struct {
	Name           string            `json:"name"`
	Template       UnitTemplate      `json:"template"`
	RequiredLabels map[string]string `json:"required_labels,omitempty"`
	DueAt          time.Time         `json:"due_at"`
	EverySeconds   int               `json:"every_seconds,omitempty"`
	MaxRuns        int               `json:"max_runs"`
	MaxAttempts    int               `json:"max_attempts"`
	RetrySeconds   int               `json:"retry_seconds"`
	Run            int               `json:"run"`
	Attempt        int               `json:"attempt"`
	ActiveTask     string            `json:"active_task,omitempty"`
	NextAt         time.Time         `json:"next_at"`
	Done           bool              `json:"done"`
	Paused         bool              `json:"paused"`
}

func (s *Store) CreateTaskSchedule(j TaskSchedule) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	if s.data.SchemaVersion < 2 {
		return fmt.Errorf("scheduled/retry Tasks require schema 2")
	}
	if !secrets.Name.MatchString(j.Name) || len(s.data.TaskSchedules) >= 32 || s.data.TaskSchedules[j.Name].Name != "" {
		return fmt.Errorf("invalid, duplicate or full schedule catalog")
	}
	if j.DueAt.IsZero() || j.DueAt.Before(time.Now().Add(-time.Minute)) || j.DueAt.After(time.Now().Add(365*24*time.Hour)) || j.MaxRuns < 1 || j.MaxRuns > 16 || j.MaxAttempts < 1 || j.MaxAttempts > 4 || j.RetrySeconds < 1 || j.RetrySeconds > 3600 || (j.EverySeconds == 0 && j.MaxRuns != 1) || (j.EverySeconds != 0 && (j.EverySeconds < 10 || j.EverySeconds > 86400)) {
		return fmt.Errorf("invalid bounded schedule/retry policy")
	}
	// Normalize and validate through ordinary Task admission in the same transaction;
	// the probe is never committed or dispatchable.
	probe := Task{Name: "schedule-validation-probe", Template: j.Template, RequiredLabels: j.RequiredLabels}
	// Admission normalization is shared below; no probe Task is published.
	normalized, e := normalizeTask(s.data, probe)
	if e != nil {
		return e
	}
	j.Template = normalized.Template
	j.RequiredLabels = normalized.RequiredLabels
	j.Run = 1
	j.Attempt = 1
	j.ActiveTask = ""
	j.NextAt = j.DueAt.UTC()
	j.Done = false
	j.Paused = false
	if s.data.TaskSchedules == nil {
		s.data.TaskSchedules = map[string]TaskSchedule{}
	}
	s.data.TaskSchedules[j.Name] = j
	return s.commitLocked()
}

func scheduleTaskName(j TaskSchedule) string {
	return "sch-" + taskIdentity("schedule", j.Name, fmt.Sprintf("%d/%d", j.Run, j.Attempt))[:28]
}

// Reconciler holds Orchestration; state and new immutable attempt commit together.
func (s *Store) DispatchScheduledTask(name string, now time.Time) error {
	s.lock()
	defer s.unlock()
	j, ok := s.data.TaskSchedules[name]
	if !ok || j.Done || j.Paused || j.ActiveTask != "" || now.Before(j.NextAt) {
		return nil
	}
	t := Task{Name: scheduleTaskName(j), Template: j.Template, RequiredLabels: j.RequiredLabels}
	j.ActiveTask = t.Name
	s.data.TaskSchedules[name] = j
	return s.createTaskLocked(t)
}

// Proof is the exact original terminal Task observed and positively stopped and
// lease-revoked on its original node by the quorum-guarded reconciler.
func (s *Store) FinishScheduledTask(name string, proof Task, now time.Time) error {
	s.lock()
	defer s.unlock()
	j, ok := s.data.TaskSchedules[name]
	t, exists := s.data.Tasks[j.ActiveTask]
	if !ok || !exists || !t.Terminal() || !reflect.DeepEqual(t, proof) || t.ExitCode == nil || t.RunID == "" || t.ExecutionID == "" {
		return fmt.Errorf("matching confirmed terminal execution and fencing acknowledgement required")
	}
	if t.Phase == TaskFailed && j.Attempt < j.MaxAttempts {
		j.Attempt++
		j.NextAt = now.Add(time.Duration(j.RetrySeconds) * time.Second)
	} else if t.Phase == TaskCancelled || j.Run >= j.MaxRuns {
		j.Done = true
	} else {
		j.Run++
		j.Attempt = 1
		j.NextAt = now.Add(time.Duration(j.EverySeconds) * time.Second)
	}
	j.ActiveTask = ""
	s.data.TaskSchedules[name] = j
	return s.commitLocked()
}
func (s *Store) PauseTaskSchedule(name string) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	j, ok := s.data.TaskSchedules[name]
	if !ok {
		return fmt.Errorf("unknown Task schedule")
	}
	j.Paused = true
	s.data.TaskSchedules[name] = j
	return s.commitLocked()
}

func ValidateOrchestrationState(s State) error {
	if len(s.TaskSchedules) > 32 || len(s.SecretRotations) > 16 {
		return fmt.Errorf("orchestration journal bounds exceeded")
	}
	for name, j := range s.TaskSchedules {
		if name != j.Name || !secrets.Name.MatchString(name) || j.Run < 1 || j.Run > j.MaxRuns || j.Attempt < 1 || j.Attempt > j.MaxAttempts || j.MaxRuns > 16 || j.MaxAttempts > 4 || j.NextAt.IsZero() || j.DueAt.IsZero() || j.RetrySeconds < 1 || j.RetrySeconds > 3600 || (j.EverySeconds == 0 && j.MaxRuns != 1) || (j.EverySeconds != 0 && (j.EverySeconds < 10 || j.EverySeconds > 86400)) {
			return fmt.Errorf("invalid retained Task schedule")
		}
		if j.ActiveTask != "" {
			t, ok := s.Tasks[j.ActiveTask]
			if !ok || t.Name != scheduleTaskName(j) || !reflect.DeepEqual(t.Template, j.Template) {
				return fmt.Errorf("schedule immutable attempt mismatch")
			}
		}
	}
	seen := map[string]bool{}
	previous := uint64(0)
	for _, r := range s.SecretRotations {
		if !migrationID.MatchString(r.ID) || seen[r.ID] || !executionID.MatchString(r.Fingerprint) || !secrets.Name.MatchString(r.Target) || r.Revision <= previous || r.Revision > s.Revision || r.CommittedAt.IsZero() {
			return fmt.Errorf("invalid durable secret rotation marker")
		}
		seen[r.ID] = true
		previous = r.Revision
	}
	return nil
}

func schedulePolicy(j TaskSchedule) TaskSchedule {
	j.Run, j.Attempt = 0, 0
	j.ActiveTask = ""
	j.NextAt = time.Time{}
	j.Done, j.Paused = false, false
	return j
}
