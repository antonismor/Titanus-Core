package security

import "testing"

func TestIsolationPolicyRejectsDowngrade(t *testing.T) {
	for _, p := range []Policy{
		{UserNamespace: "host"}, {Devices: "unconfined"}, {LSM: "none"}, {LSM: "apparmor"},
		{LSMWritePaths: []string{"/"}}, {LSMWritePaths: []string{"/proc/self"}},
		{LSMWritePaths: []string{"/dev"}}, {LSMWritePaths: []string{"/sys"}},
		{LSMWritePaths: []string{"/tmp/../proc"}}, {RunAsUID: 65536}, {RunAsGID: 65536},
	} {
		if e := p.Validate(); e == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}

// Interpret the small emitted eBPF subset to prove the allowlist has exact
// major/minor/access matching (including adversarial adjacent minors).
func TestDeviceBytecode(t *testing.T) {
	run := func(kind, access, major, minor int32) int32 {
		ctx := [3]int32{kind | access<<16, major, minor}
		var regs [11]int32
		p := deviceProgram()
		for pc := 0; pc < len(p); pc++ {
			i := p[pc]
			r := i.Regs & 15
			switch i.Code {
			case 0x61:
				regs[r] = ctx[i.Imm/4]
			case 0x57:
				regs[r] &= i.Imm
			case 0x77:
				regs[r] = int32(uint32(regs[r]) >> uint32(i.Imm))
			case 0x55:
				if regs[r] != i.Imm {
					pc += int(i.Off)
				}
			case 0x15:
				if regs[r] == i.Imm {
					pc += int(i.Off)
				}
			case 0xb5:
				if uint32(regs[r]) <= uint32(i.Imm) {
					pc += int(i.Off)
				}
			case 0xb7:
				regs[r] = i.Imm
			case 0x95:
				return regs[0]
			default:
				t.Fatalf("unknown opcode %x", i.Code)
			}
		}
		t.Fatal("no exit")
		return -1
	}
	for _, d := range [][2]int32{{1, 3}, {1, 5}, {1, 7}, {1, 8}, {1, 9}, {5, 0}, {5, 2}, {136, 0}, {136, 255}} {
		for _, access := range []int32{2, 4, 6} {
			if run(2, access, d[0], d[1]) != 1 {
				t.Fatalf("denied %v/%d", d, access)
			}
		}
		if run(2, 1, d[0], d[1]) != 0 || run(2, 7, d[0], d[1]) != 0 || run(1, 6, d[0], d[1]) != 0 {
			t.Fatal("creation/block bypass")
		}
	}
	for _, d := range [][2]int32{{1, 1}, {1, 11}, {1, 4}, {1, 6}, {5, 1}, {7, 0}, {8, 0}, {136, 256}, {137, 0}, {0, 0}} {
		if run(2, 6, d[0], d[1]) != 0 {
			t.Fatalf("allowed %v", d)
		}
	}
}

func TestDeviceAttachFailsClosedWithoutCgroup(t *testing.T) {
	// On restricted hosts the load itself fails; on privileged hosts the invalid
	// target must reject attachment. Neither case may report enforcement success.
	if err := EnforceDevices(t.TempDir()); err == nil {
		t.Fatal("reported enforcement without a cgroup")
	}
}
