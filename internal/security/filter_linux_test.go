package security

import (
	"syscall"
	"testing"
)

// Evaluate the cBPF program against synthetic seccomp_data, independently of
// host privileges. Kernel tests below validate the same bytes against Linux.
func evaluate(t *testing.T, filter []syscall.SockFilter, arch, nr, flags uint32) uint32 {
	t.Helper()
	var a uint32
	for pc := 0; pc < len(filter); pc++ {
		i := filter[pc]
		switch i.Code {
		case 0x20:
			switch i.K {
			case 0:
				a = nr
			case 4:
				a = arch
			case 16:
				a = flags
			default:
				t.Fatalf("unknown load %d", i.K)
			}
		case 0x15:
			if a == i.K {
				pc += int(i.Jt)
			} else {
				pc += int(i.Jf)
			}
		case 0x35:
			if a >= i.K {
				pc += int(i.Jt)
			} else {
				pc += int(i.Jf)
			}
		case 0x45:
			if a&i.K != 0 {
				pc += int(i.Jt)
			} else {
				pc += int(i.Jf)
			}
		case 0x06:
			return i.K
		default:
			t.Fatalf("unknown instruction %x", i.Code)
		}
	}
	t.Fatal("filter did not terminate")
	return 0
}

func TestDefaultFilterBoundaries(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			f, err := DefaultFilter(arch)
			if err != nil {
				t.Fatal(err)
			}
			p := amd64Profile()
			if arch == "arm64" {
				p = arm64Profile()
			}
			for _, nr := range p.allowed {
				if got := evaluate(t, f, p.audit, nr, 0); got != actionAllow {
					t.Fatalf("allowed syscall %d got %x", nr, got)
				}
			}
			blocked := []uint32{101, 165, 166, 169, 175, 176, 246, 248, 249, 250, 272, 298, 304, 308, 313, 321, 323}
			if arch == "arm64" {
				blocked = []uint32{39, 40, 41, 97, 104, 105, 106, 117, 142, 217, 219, 241, 264, 265, 268, 280, 282}
			}
			blocked = append(blocked, 425, 426, 427, 428, 429, 430, 431, 432, 442, 9999)
			for _, nr := range blocked {
				if got := evaluate(t, f, p.audit, nr, 0); got != actionDenied {
					t.Fatalf("dangerous syscall %d got %x", nr, got)
				}
			}
			if got := evaluate(t, f, 0x40000003, 0, 0); got != actionKill {
				t.Fatal("foreign ABI allowed")
			}
			if arch == "amd64" {
				for _, nr := range []uint32{0x40000000, 0x40000001, 512, 547} {
					if evaluate(t, f, p.audit, nr, 0) == actionAllow {
						t.Fatal("x32 bypass")
					}
				}
			}
			if evaluate(t, f, p.audit, p.clone3, 0) != actionNoSys {
				t.Fatal("clone3 fallback missing")
			}
			if evaluate(t, f, p.audit, p.clone, 0x100|0x10000) != actionAllow {
				t.Fatal("ordinary thread creation denied")
			}
			for _, flag := range []uint32{0x80, 0x20000, 0x2000000, 0x4000000, 0x8000000, 0x10000000, 0x20000000, 0x40000000} {
				if evaluate(t, f, p.audit, p.clone, flag) != actionDenied {
					t.Fatalf("namespace clone %x allowed", flag)
				}
			}
		})
	}
	if _, err := DefaultFilter("386"); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
}
