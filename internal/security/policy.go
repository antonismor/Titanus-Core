// Package security defines Titanus' native workload security policy.
package security

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const DefaultProfile = "titanus-default-v1"

// Policy has no privileged bypass. A missing policy is hardened on first start.
// Pointer semantics distinguish an omitted no_new_privs from an explicit false.
type Policy struct {
	LSM             string   `json:"lsm"`
	LSMWritePaths   []string `json:"lsm_write_paths,omitempty"`
	UserNamespace   string   `json:"user_namespace"`
	Devices         string   `json:"devices"`
	Profile         string   `json:"profile,omitempty"`
	RunAsUID        int      `json:"run_as_uid,omitempty"`
	RunAsGID        int      `json:"run_as_gid,omitempty"`
	ReadOnlyRootFS  bool     `json:"read_only_rootfs,omitempty"`
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
	p.Profile = NormalizeProfile(p.Profile)
	if p.UserNamespace == "" {
		p.UserNamespace = "mapped-v1"
	}
	if p.Devices == "" {
		p.Devices = "safe-v1"
	}
	if p.LSM == "" {
		p.LSM = "landlock-v1"
	}
	if p.LSMWritePaths == nil {
		p.LSMWritePaths = []string{"/tmp"}
	}
	p.LSMWritePaths = append([]string{}, p.LSMWritePaths...)
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
	if p.UserNamespace != "mapped-v1" || p.Devices != "safe-v1" || p.LSM != "landlock-v1" {
		return fmt.Errorf("mapped-v1 user namespace, safe-v1 devices and landlock-v1 LSM are mandatory")
	}
	if p.RunAsUID >= 65536 || p.RunAsGID >= 65536 {
		return fmt.Errorf("workload UID/GID must fit the 65536-ID mapping")
	}
	seenPaths := map[string]bool{}
	for _, path := range p.LSMWritePaths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\n") {
			return fmt.Errorf("invalid LSM writable path %q", path)
		}
		for _, protected := range []string{"/proc", "/sys", "/dev", "/.titanus-oldroot"} {
			if path == protected || strings.HasPrefix(path, protected+"/") {
				return fmt.Errorf("protected LSM path %q", path)
			}
		}
		if seenPaths[path] {
			return fmt.Errorf("duplicate LSM writable path %q", path)
		}
		seenPaths[path] = true
	}
	if err := ValidateProfile(p.Profile); err != nil {
		return err
	}
	if p.RunAsUID < 0 || uint64(p.RunAsUID) >= 0xffffffff {
		return fmt.Errorf("invalid run_as_uid")
	}
	if p.RunAsGID < 0 || uint64(p.RunAsGID) >= 0xffffffff {
		return fmt.Errorf("invalid run_as_gid")
	}
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
