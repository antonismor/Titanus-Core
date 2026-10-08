package unitruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Usage struct {
	RunID       string    `json:"run_id"`
	CPUUsec     uint64    `json:"cpu_usec"`
	MemoryBytes uint64    `json:"memory_bytes"`
	Time        time.Time `json:"time"`
}

func (m *Manager) Usage(id string) (Usage, error) {
	unlock, e := m.lock()
	if e != nil {
		return Usage{}, e
	}
	defer unlock()
	_, s, e := m.load(id)
	if e != nil {
		return Usage{}, e
	}
	if s.Status != StatusActive || !processMatches(s) {
		return Usage{}, fmt.Errorf("Unit is not live")
	}
	cpu, e := os.ReadFile(filepath.Join(m.cgroupDir(id), "cpu.stat"))
	if e != nil {
		return Usage{}, e
	}
	mem, e := os.ReadFile(filepath.Join(m.cgroupDir(id), "memory.current"))
	if e != nil {
		return Usage{}, e
	}
	v, e := strconv.ParseUint(strings.TrimSpace(string(mem)), 10, 64)
	if e != nil {
		return Usage{}, fmt.Errorf("invalid memory counter")
	}
	var use uint64
	found := false
	for _, line := range strings.Split(string(cpu), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "usage_usec" {
			use, e = strconv.ParseUint(f[1], 10, 64)
			if e != nil {
				return Usage{}, fmt.Errorf("invalid CPU counter")
			}
			found = true
		}
	}
	if !found {
		return Usage{}, fmt.Errorf("CPU usage counter unavailable")
	}
	return Usage{RunID: s.RunID, CPUUsec: use, MemoryBytes: v, Time: time.Now().UTC()}, nil
}
