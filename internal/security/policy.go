// Package security defines Titanus' native workload security policy.
package security

import (
	"fmt"
	"sort"
	"strings"
)

const DefaultProfile = "titanus-default-v1"

// Policy has no privileged bypass. A missing policy is hardened on first start.
// Pointer semantics distinguish an omitted no_new_privs from an explicit false.
type Policy struct {
	Seccomp         string   `json:"seccomp"`
	NoNewPrivileges *bool    `json:"no_new_privs,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

// Only capabilities needed by ordinary applications may be requested. Kernel,
// namespace, device, network administration and capability administration are
// deliberately excluded from this first workload profile.
var applicationCaps = map[string]uint{
	"CAP_CHOWN": 0, "CAP_DAC_OVERRIDE": 1, "CAP_FOWNER": 3,
	"CAP_FSETID": 4, "CAP_KILL": 5, "CAP_SETGID": 6,
	"CAP_SETUID": 7, "CAP_NET_BIND_SERVICE": 10,
}

func (p *Policy) Normalize() {
	p.Capabilities = append([]string(nil), p.Capabilities...)
	if p.Seccomp == "" {
		p.Seccomp = DefaultProfile
	}
	if p.NoNewPrivileges == nil {
		value := true
		p.NoNewPrivileges = &value
	}
	if len(p.Capabilities) == 0 {
		p.Capabilities = nil
	}
	for i, c := range p.Capabilities {
		c = strings.ToUpper(strings.TrimSpace(c))
		if !strings.HasPrefix(c, "CAP_") {
			c = "CAP_" + c
		}
		p.Capabilities[i] = c
	}
	sort.Strings(p.Capabilities)
}

func (p Policy) Validate() error {
	p.Normalize()
	if p.Seccomp != DefaultProfile {
		return fmt.Errorf("unsupported seccomp profile %q", p.Seccomp)
	}
	if !*p.NoNewPrivileges {
		return fmt.Errorf("no_new_privs must be enabled")
	}
	seen := map[string]bool{}
	for _, c := range p.Capabilities {
		if _, ok := applicationCaps[c]; !ok {
			return fmt.Errorf("unsupported workload capability %q", c)
		}
		if seen[c] {
			return fmt.Errorf("duplicate workload capability %q", c)
		}
		seen[c] = true
	}
	return nil
}

func (p Policy) capabilityMask() uint64 {
	var mask uint64
	for _, c := range p.Capabilities {
		mask |= uint64(1) << applicationCaps[c]
	}
	return mask
}
