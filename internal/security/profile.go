package security

import (
	"fmt"
	"sort"
	"strings"
)

const (
	ProfileRestricted = "restricted"
	ProfileUnconfined = "unconfined"
)

type Profile struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	NoNewPrivileges  bool   `json:"no_new_privileges"`
	DropCapabilities bool   `json:"drop_capabilities"`
	Seccomp          bool   `json:"seccomp"`
}

type Spec struct {
	Profile          string `json:"profile,omitempty"`
	RunAsUID         int    `json:"run_as_uid,omitempty"`
	RunAsGID         int    `json:"run_as_gid,omitempty"`
	ReadOnlyRootFS   bool   `json:"read_only_rootfs,omitempty"`
}

var builtins = map[string]Profile{
	ProfileRestricted: {
		Name: ProfileRestricted,
		Description: "Safe Titanus default: no-new-privileges, empty capability sets and a kernel seccomp denylist for privileged host-control syscalls.",
		NoNewPrivileges: true,
		DropCapabilities: true,
		Seccomp: true,
	},
	ProfileUnconfined: {
		Name: ProfileUnconfined,
		Description: "Compatibility profile without Titanus capability or seccomp restrictions. Use only for trusted workloads that require host-level Linux privileges.",
	},
}

func (s *Spec) Normalize() {
	s.Profile = NormalizeProfile(s.Profile)
}

func (s Spec) Validate() error {
	if err := ValidateProfile(s.Profile); err != nil {
		return err
	}
	if s.RunAsUID < 0 {
		return fmt.Errorf("run_as_uid cannot be negative")
	}
	if s.RunAsGID < 0 {
		return fmt.Errorf("run_as_gid cannot be negative")
	}
	return nil
}

func NormalizeProfile(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ProfileRestricted
	}
	return value
}

func ValidateProfile(value string) error {
	name := NormalizeProfile(value)
	if _, ok := builtins[name]; !ok {
		return fmt.Errorf("unknown Titanus Security Profile %q", value)
	}
	return nil
}

func GetProfile(value string) (Profile, bool) {
	profile, ok := builtins[NormalizeProfile(value)]
	return profile, ok
}

func BuiltinProfiles() []Profile {
	names := make([]string, 0, len(builtins))
	for name := range builtins {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Profile, 0, len(names))
	for _, name := range names {
		out = append(out, builtins[name])
	}
	return out
}
