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

// Spec preserves the security specification API introduced by PR #6.
type Spec = Policy

var builtins = map[string]Profile{
	ProfileRestricted: {
		Name:             ProfileRestricted,
		Description:      "Titanus default: no-new-privileges, explicit application capabilities and a native seccomp allowlist.",
		NoNewPrivileges:  true,
		DropCapabilities: true,
		Seccomp:          true,
	},
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
