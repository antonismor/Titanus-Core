package fencing

import (
	"errors"
	"strings"
	"testing"
)

func TestPowerFenceVerifiesUUIDAndConfirmedOff(t *testing.T) {
	target := Target{Provider: "libvirt", UUID: "01234567-89ab-cdef-0123-456789abcdef", Address: "qemu:///system"}
	off := false
	destroys := 0
	observations := 0
	p := &Power{Targets: map[string]Target{"old": target}, run: func(_ string, args ...string) (string, error) {
		switch args[len(args)-2] {
		case "domuuid":
			return target.UUID, nil
		case "destroy":
			destroys++
			off = true
			return "destroyed", nil
		case "domstate":
			observations++
			if off {
				return "shut off", nil
			}
			return "running", nil
		}
		return "", errors.New("unexpected command")
	}}
	if err := p.Fence("old", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if destroys != 1 || observations < 3 {
		t.Fatal("did not prove power-off", destroys, observations)
	}
	if err := p.Fence("old", func() error { return nil }); err != nil {
		t.Fatal("idempotent fence", err)
	}
	if destroys != 1 {
		t.Fatal("repeated already-confirmed power off")
	}
	p.run = func(_ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "destroy") {
			t.Fatal("acted before UUID validation")
		}
		return "another-uuid", nil
	}
	if err := p.Fence("old", func() error { return nil }); err == nil {
		t.Fatal("wrong target fenced")
	}
}
func TestPowerFenceLostQuorumStopsBeforeMutation(t *testing.T) {
	p := &Power{Targets: map[string]Target{"old": {}}, run: func(string, ...string) (string, error) { t.Fatal("command without quorum"); return "", nil }}
	denied := errors.New("lost quorum")
	if err := p.Fence("old", func() error { return denied }); !errors.Is(err, denied) {
		t.Fatal(err)
	}
}
