package unitruntime

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

type Probe struct {
	Protocol            string `json:"protocol"`
	Port                int    `json:"port"`
	Path                string `json:"path,omitempty"`
	IntervalSeconds     int    `json:"interval_seconds"`
	TimeoutSeconds      int    `json:"timeout_seconds"`
	InitialDelaySeconds int    `json:"initial_delay_seconds,omitempty"`
	SuccessThreshold    int    `json:"success_threshold"`
	FailureThreshold    int    `json:"failure_threshold"`
}

type Health struct {
	Readiness             *Probe `json:"readiness,omitempty"`
	Liveness              *Probe `json:"liveness,omitempty"`
	Restart               string `json:"restart"`
	InitialBackoffSeconds int    `json:"initial_backoff_seconds"`
	MaxBackoffSeconds     int    `json:"max_backoff_seconds"`
	MaxRestarts           int    `json:"max_restarts,omitempty"`
}

type ProbeResult struct {
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Successes int       `json:"successes"`
	Failures  int       `json:"failures"`
	LastError string    `json:"last_error,omitempty"`
}

func (h *Health) Normalize(defaultRestart string) {
	if h.Readiness != nil {
		probe := *h.Readiness
		h.Readiness = &probe
	}
	if h.Liveness != nil {
		probe := *h.Liveness
		h.Liveness = &probe
	}
	if h.Restart == "" {
		h.Restart = defaultRestart
	}
	if h.InitialBackoffSeconds == 0 {
		h.InitialBackoffSeconds = 2
	}
	if h.MaxBackoffSeconds == 0 {
		h.MaxBackoffSeconds = 60
	}
	for _, p := range []*Probe{h.Readiness, h.Liveness} {
		if p == nil {
			continue
		}
		if p.IntervalSeconds == 0 {
			p.IntervalSeconds = 5
		}
		if p.TimeoutSeconds == 0 {
			p.TimeoutSeconds = 2
		}
		if p.SuccessThreshold == 0 {
			p.SuccessThreshold = 1
		}
		if p.FailureThreshold == 0 {
			p.FailureThreshold = 3
		}
		if p.Protocol == "http" && p.Path == "" {
			p.Path = "/"
		}
	}
}

func (h Health) Validate() error {
	if h.Restart != "never" && h.Restart != "on-failure" && h.Restart != "always" {
		return fmt.Errorf("restart must be never, on-failure or always")
	}
	if h.InitialBackoffSeconds < 1 || h.MaxBackoffSeconds < h.InitialBackoffSeconds || h.MaxBackoffSeconds > 3600 || h.MaxRestarts < 0 {
		return fmt.Errorf("invalid restart backoff/limit")
	}
	for _, p := range []*Probe{h.Readiness, h.Liveness} {
		if p == nil {
			continue
		}
		if p.Protocol != "http" && p.Protocol != "tcp" || p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("probe requires HTTP/TCP and a valid port")
		}
		if p.IntervalSeconds < 1 || p.IntervalSeconds > 300 || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 30 || p.InitialDelaySeconds < 0 || p.InitialDelaySeconds > 3600 || p.SuccessThreshold < 1 || p.SuccessThreshold > 10 || p.FailureThreshold < 1 || p.FailureThreshold > 10 {
			return fmt.Errorf("probe timing/threshold out of range")
		}
		if p.Protocol == "http" {
			u, err := url.ParseRequestURI(p.Path)
			if err != nil || u.IsAbs() || u.Host != "" || len(p.Path) == 0 || p.Path[0] != '/' || len(p.Path) > 2048 {
				return fmt.Errorf("HTTP probe path must be a local absolute path")
			}
		}
	}
	return nil
}

func ParseProbe(raw string) (*Probe, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() != "127.0.0.1" || u.Fragment != "" {
		return nil, fmt.Errorf("probe URL must target 127.0.0.1 inside the Unit")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return nil, fmt.Errorf("probe URL requires a port")
	}
	p := &Probe{Protocol: u.Scheme, Port: port, Path: u.RequestURI()}
	h := Health{Readiness: p}
	h.Normalize("never")
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func advanceProbe(result ProbeResult, err error, now time.Time) ProbeResult {
	result.CheckedAt = now
	if err == nil {
		if result.Successes < 10 {
			result.Successes++
		}
		result.Failures = 0
		result.LastError = ""
	} else {
		if result.Failures < 10 {
			result.Failures++
		}
		result.Successes = 0
		result.LastError = err.Error()
	}
	return result
}

func restartDue(spec Spec, state *State, now time.Time) bool {
	if !state.DesiredRunning || state.PID > 0 || spec.Health.Restart == "never" || state.LastError == "restart limit reached" {
		return false
	}
	failed := state.HealthFailed || state.Status == StatusFailed || state.ExitCode != nil && *state.ExitCode != 0
	if spec.Health.Restart == "on-failure" && !failed {
		return false
	}
	if spec.Health.MaxRestarts > 0 && state.RestartCount >= spec.Health.MaxRestarts {
		state.LastError = "restart limit reached"
		return false
	}
	if state.NextStartAt.IsZero() {
		delay := time.Duration(spec.Health.InitialBackoffSeconds) * time.Second
		for i := 0; i < state.RestartCount && delay < time.Duration(spec.Health.MaxBackoffSeconds)*time.Second; i++ {
			delay *= 2
		}
		if delay > time.Duration(spec.Health.MaxBackoffSeconds)*time.Second {
			delay = time.Duration(spec.Health.MaxBackoffSeconds) * time.Second
		}
		state.NextStartAt = now.Add(delay)
		return false
	}
	return !now.Before(state.NextStartAt)
}
