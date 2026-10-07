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
)

var objectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type Spec struct {
	ID          string   `json:"id"`
	Source      string   `json:"source"`
	Hostname    string   `json:"hostname"`
	Command     []string `json:"command"`
	Environment []string `json:"environment,omitempty"`
	MemoryBytes int64    `json:"memory_bytes"`
	CPUPercent  int      `json:"cpu_percent"`
	PidsMax     int      `json:"pids_max"`
}

type Status string

const (
	StatusCreated Status = "CREATED"
	StatusStarting Status = "STARTING"
	StatusActive   Status = "ACTIVE"
	StatusStopped  Status = "STOPPED"
	StatusFailed   Status = "FAILED"
)

type State struct {
	ID        string    `json:"id"`
	Status    Status    `json:"status"`
	PID       int       `json:"pid,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	StartedAt time.Time `json:"started_at,omitempty"`
	StoppedAt time.Time `json:"stopped_at,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

func (s *Spec) Normalize() {
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
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadJSON(path string, value any) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
