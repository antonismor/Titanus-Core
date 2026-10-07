package unitruntime

import (
	"testing"

	"github.com/antonismor/Titanus-Core/internal/security"
)

func TestParseBytes(t *testing.T) {
	tests := map[string]int64{
		"512M":    512 * 1024 * 1024,
		"1GiB":    1024 * 1024 * 1024,
		"2GB":     2 * 1000 * 1000 * 1000,
		"1048576": 1048576,
	}
	for input, expected := range tests {
		got, err := ParseBytes(input)
		if err != nil {
			t.Fatalf("ParseBytes(%q): %v", input, err)
		}
		if got != expected {
			t.Fatalf("ParseBytes(%q)=%d, want %d", input, got, expected)
		}
	}
}

func TestSpecValidation(t *testing.T) {
	s := Spec{ID: "web01", Source: "debian13", Command: []string{"/bin/sh"}}
	s.Normalize()
	if err := s.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

func TestSpecDefaultsToRestrictedSecurity(t *testing.T) {
	spec := Spec{ID: "secure01", Source: "busybox", Command: []string{"/bin/sh"}}
	spec.Normalize()
	if spec.Security.Profile != security.ProfileRestricted {
		t.Fatalf("expected restricted Security Profile, got %#v", spec.Security)
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSpecRejectsUnknownSecurityProfile(t *testing.T) {
	spec := Spec{ID: "secure02", Source: "busybox", Command: []string{"/bin/sh"}}
	spec.Normalize()
	spec.Security.Profile = "unknown"
	if err := spec.Validate(); err == nil {
		t.Fatal("expected invalid Security Profile to be rejected")
	}
}
