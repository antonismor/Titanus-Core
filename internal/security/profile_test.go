package security

import "testing"

func TestNormalizeAndValidateProfiles(t *testing.T) {
	if got := NormalizeProfile(""); got != ProfileRestricted {
		t.Fatalf("expected restricted default, got %s", got)
	}
	if err := ValidateProfile("restricted"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProfile("UNCONFINED"); err == nil {
		t.Fatal("unconfined profile must not bypass workload hardening")
	}
	if err := ValidateProfile("missing"); err == nil {
		t.Fatal("expected unknown profile to be rejected")
	}
}

func TestBuiltinProfilesExposeSafeDefault(t *testing.T) {
	profiles := BuiltinProfiles()
	if len(profiles) != 1 {
		t.Fatalf("unexpected built-in profiles %#v", profiles)
	}
	profile, ok := GetProfile(ProfileRestricted)
	if !ok || !profile.NoNewPrivileges || !profile.DropCapabilities || !profile.Seccomp {
		t.Fatalf("restricted profile is not fully hardened: %#v", profile)
	}
}

func TestSecuritySpecDefaultsAndValidation(t *testing.T) {
	spec := Spec{}
	spec.Normalize()
	if spec.Profile != ProfileRestricted {
		t.Fatalf("expected restricted default, got %#v", spec)
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Spec{Profile: ProfileRestricted, RunAsUID: -1}).Validate(); err == nil {
		t.Fatal("expected negative UID to be rejected")
	}
	if err := (Spec{Profile: "missing"}).Validate(); err == nil {
		t.Fatal("expected unknown profile to be rejected")
	}
}
