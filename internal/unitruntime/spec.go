package unitruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/fabric"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/antonismor/Titanus-Core/internal/security"
)

var objectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type NetworkSpec struct {
	Fabric bool          `json:"fabric"`
	Ports  []fabric.Port `json:"ports,omitempty"`
}

type Spec struct {
	SecretRealm    string            `json:"secret_realm,omitempty"`
	Secrets        []secrets.Ref     `json:"secrets,omitempty"`
	SecretBindings []secrets.Binding `json:"secret_bindings,omitempty"`
	ID             string            `json:"id"`
	Source         string            `json:"source"`
	Hostname       string            `json:"hostname"`
	Command        []string          `json:"command"`
	Environment    []string          `json:"environment,omitempty"`
	MemoryBytes    int64             `json:"memory_bytes"`
	CPUPercent     int               `json:"cpu_percent"`
	PidsMax        int               `json:"pids_max"`
	Network        NetworkSpec       `json:"network"`
	Mounts         []disk.Mount      `json:"mounts,omitempty"`
	Health         Health            `json:"health"`
	Security       security.Policy   `json:"security"`
}

type Status string

const (
	StatusCreated  Status = "CREATED"
	StatusStarting Status = "STARTING"
	StatusActive   Status = "ACTIVE"
	StatusStopped  Status = "STOPPED"
	StatusFailed   Status = "FAILED"
)

type State struct {
	HealthFailed   bool        `json:"health_failed,omitempty"`
	DesiredRunning bool        `json:"desired_running"`
	Ready          bool        `json:"ready"`
	Live           bool        `json:"live"`
	Readiness      ProbeResult `json:"readiness"`
	Liveness       ProbeResult `json:"liveness"`
	RestartCount   int         `json:"restart_count"`
	NextStartAt    time.Time   `json:"next_start_at,omitempty"`

	UserMapping    IDMapping       `json:"user_mapping"`
	ID             string          `json:"id"`
	Status         Status          `json:"status"`
	Process        ProcessIdentity `json:"process"`
	RunID          string          `json:"run_id,omitempty"`
	ExitCode       *int            `json:"exit_code,omitempty"`
	ExitSignal     int             `json:"exit_signal,omitempty"`
	PID            int             `json:"pid,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	StartedAt      time.Time       `json:"started_at,omitempty"`
	StoppedAt      time.Time       `json:"stopped_at,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	NetworkAddress string          `json:"network_address,omitempty"`
}

func (s *Spec) Normalize() {
	s.Security.Normalize()
	s.Health.Normalize("never")
	s.ID = strings.TrimSpace(s.ID)
	s.Source = strings.TrimSpace(s.Source)
	if strings.TrimSpace(s.Hostname) == "" {
		s.Hostname = s.ID
	}
	if s.MemoryBytes == 0 {
		s.MemoryBytes = 512 * 1024 * 1024
	}
	if s.CPUPercent == 0 {
		s.CPUPercent = 100
	}
	if s.PidsMax == 0 {
		s.PidsMax = 256
	}
}

func (s Spec) Validate() error {
	if err := secrets.ValidateRefs(s.Secrets, s.Environment); err != nil {
		return err
	}
	if len(s.Secrets) != len(s.SecretBindings) || (len(s.Secrets) > 0 && s.SecretRealm == "") {
		return fmt.Errorf("incomplete encrypted secrets")
	}
	if err := s.Health.Validate(); err != nil {
		return fmt.Errorf("Unit health: %w", err)
	}
	if err := s.Security.Validate(); err != nil {
		return fmt.Errorf("Unit %s security: %w", s.ID, err)
	}
	if !objectName.MatchString(s.ID) {
		return fmt.Errorf("invalid Unit ID %q", s.ID)
	}
	if !objectName.MatchString(s.Source) {
		return fmt.Errorf("invalid Source name %q", s.Source)
	}
	if len(s.Command) == 0 || strings.TrimSpace(s.Command[0]) == "" {
		return fmt.Errorf("Unit %s requires a command", s.ID)
	}
	if s.MemoryBytes < 16*1024*1024 {
		return fmt.Errorf("Unit %s memory limit must be at least 16 MiB", s.ID)
	}
	if s.CPUPercent < 1 || s.CPUPercent > 10000 {
		return fmt.Errorf("Unit %s CPU percent must be between 1 and 10000", s.ID)
	}
	if s.PidsMax < 8 || s.PidsMax > 1048576 {
		return fmt.Errorf("Unit %s pids limit is outside the supported range", s.ID)
	}
	if err := fabric.ValidatePorts(s.Network.Ports); err != nil {
		return fmt.Errorf("Unit %s network: %w", s.ID, err)
	}
	if len(s.Network.Ports) > 0 && !s.Network.Fabric {
		return fmt.Errorf("Unit %s publishes ports but Titanus Fabric is disabled", s.ID)
	}
	if err := disk.ValidateMounts(s.Mounts); err != nil {
		return fmt.Errorf("Unit %s storage: %w", s.ID, err)
	}
	return nil
}

func ParseBytes(value string) (int64, error) {
	v := strings.TrimSpace(strings.ToUpper(value))
	if v == "" {
		return 0, fmt.Errorf("empty size")
	}

	multipliers := []struct {
		suffix string
		value  int64
	}{
		{"TIB", 1024 * 1024 * 1024 * 1024},
		{"GIB", 1024 * 1024 * 1024},
		{"MIB", 1024 * 1024},
		{"KIB", 1024},
		{"TB", 1000 * 1000 * 1000 * 1000},
		{"GB", 1000 * 1000 * 1000},
		{"MB", 1000 * 1000},
		{"KB", 1000},
		{"T", 1024 * 1024 * 1024 * 1024},
		{"G", 1024 * 1024 * 1024},
		{"M", 1024 * 1024},
		{"K", 1024},
	}

	for _, item := range multipliers {
		if strings.HasSuffix(v, item.suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(v, item.suffix))
			n, err := strconv.ParseInt(number, 10, 64)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid size %q", value)
			}
			return n * item.value, nil
		}
	}

	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	return n, nil
}

func saveJSON(path string, value any, mode os.FileMode) error {
	return durable.WriteJSON(path, value, mode)
}

func loadJSON(path string, value any) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
