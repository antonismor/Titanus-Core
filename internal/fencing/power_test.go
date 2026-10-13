package fencing

import (
	"encoding/json"
	"errors"
	"net/http"
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


func TestPowerFenceProxmoxVerifiesUUIDAndConfirmedOff(t *testing.T) {
	target := Target{
		Provider: "proxmox",
		UUID:     "01234567-89ab-cdef-0123-456789abcdef",
		Address:  "https://pve.example:8006/api2/json/nodes/pve/qemu/1003",
		User:     "titanus-fencer@pve!gateway",
	}
	off := false
	stops := 0
	observations := 0
	p := &Power{Targets: map[string]Target{"old": target}}
	p.request = func(_ Target, method, suffix string) (json.RawMessage, error) {
		switch suffix {
		case "/config":
			if method != http.MethodGet {
				t.Fatalf("unexpected config method %s", method)
			}
			return json.RawMessage("{\"smbios1\":\"uuid=01234567-89ab-cdef-0123-456789abcdef,manufacturer=Titanus\"}"), nil
		case "/status/current":
			if method != http.MethodGet {
				t.Fatalf("unexpected status method %s", method)
			}
			observations++
			if off {
				return json.RawMessage("{\"status\":\"stopped\"}"), nil
			}
			return json.RawMessage("{\"status\":\"running\"}"), nil
		case "/status/stop":
			if method != http.MethodPost {
				t.Fatalf("unexpected stop method %s", method)
			}
			stops++
			off = true
			return json.RawMessage("\"UPID:pve:stop\""), nil
		default:
			return nil, errors.New("unexpected Proxmox API path")
		}
	}
	if err := p.Fence("old", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if stops != 1 || observations < 3 {
		t.Fatal("did not prove Proxmox power-off", stops, observations)
	}
	if err := p.Fence("old", func() error { return nil }); err != nil {
		t.Fatal("idempotent Proxmox fence", err)
	}
	if stops != 1 {
		t.Fatal("repeated already-confirmed Proxmox power off")
	}
	p.request = func(_ Target, method, suffix string) (json.RawMessage, error) {
		if method == http.MethodPost {
			t.Fatal("acted before Proxmox UUID validation")
		}
		if suffix == "/config" {
			return json.RawMessage("{\"smbios1\":\"uuid=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\"}"), nil
		}
		return json.RawMessage("{\"status\":\"running\"}"), nil
	}
	if err := p.Fence("old", func() error { return nil }); err == nil {
		t.Fatal("wrong Proxmox VM fenced")
	}
}

func TestPowerFenceProxmoxLostQuorumStopsBeforeMutation(t *testing.T) {
	target := Target{
		Provider: "proxmox",
		UUID:     "01234567-89ab-cdef-0123-456789abcdef",
		Address:  "https://pve.example:8006/api2/json/nodes/pve/qemu/1003",
		User:     "titanus-fencer@pve!gateway",
	}
	requests := 0
	p := &Power{Targets: map[string]Target{"old": target}, request: func(Target, string, string) (json.RawMessage, error) {
		requests++
		return nil, errors.New("request should not run")
	}}
	denied := errors.New("lost quorum")
	if err := p.Fence("old", func() error { return denied }); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatal("Proxmox API contacted without quorum")
	}
}
