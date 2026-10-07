//go:build linux

package security

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

type deviceInsn struct {
	Code, Regs uint8
	Off        int16
	Imm        int32
}

// Linux BPF_CGROUP_DEVICE ctx: access_type, major, minor. Every request is
// denied except read/write of fixed harmless character devices and PTYs.
// mknod and all block devices are denied, including nodes in mounted Disks.
func deviceProgram() []deviceInsn {
	p := []deviceInsn{
		{0x61, 0x12, 0, 0}, // r2 = ctx.access_type
		{0x57, 0x02, 0, 0xffff},
		{0x55, 0x02, 0, 2}, // only CHAR (2)
		{0x61, 0x12, 0, 0},
		{0x77, 0x02, 0, 16},
		{0x57, 0x02, 0, 1},
		{0x55, 0x02, 0, 0}, // no MKNOD
		{0x61, 0x12, 4, 0}, // major
		{0x61, 0x13, 8, 0}, // minor
	}
	type pair struct{ major, minor int32 }
	for _, d := range []pair{{1, 3}, {1, 5}, {1, 7}, {1, 8}, {1, 9}, {5, 0}, {5, 2}} {
		p = append(p, deviceInsn{0x55, 0x02, 1, d.major}, deviceInsn{0x15, 0x03, 0, d.minor})
	}
	p = append(p, deviceInsn{0x55, 0x02, 1, 136}, deviceInsn{0xb5, 0x03, 0, 255}) // PTY minor <=255
	deny := len(p)
	p = append(p, deviceInsn{0xb7, 0x00, 0, 0}, deviceInsn{0x95, 0, 0, 0})
	allow := len(p)
	p = append(p, deviceInsn{0xb7, 0, 0, 1}, deviceInsn{0x95, 0, 0, 0})
	p[2].Off = int16(deny - 3)
	p[6].Off = int16(deny - 7)
	for i := 10; i < deny; i += 2 {
		p[i].Off = int16(allow - i - 1)
	}
	return p
}

func bpfCall(cmd uintptr, attr []byte) (uintptr, error) {
	nr := uintptr(321)
	if runtime.GOARCH == "arm64" {
		nr = 280
	} else if runtime.GOARCH != "amd64" {
		return 0, fmt.Errorf("unsupported BPF architecture")
	}
	r, _, e := syscall.Syscall(nr, cmd, uintptr(unsafe.Pointer(&attr[0])), uintptr(len(attr)))
	runtime.KeepAlive(attr)
	if e != 0 {
		return 0, e
	}
	return r, nil
}

func EnforceDevices(cgroup string) error {
	group, e := os.Open(cgroup)
	if e != nil {
		return e
	}
	defer group.Close()
	insns := deviceProgram()
	license := []byte("GPL\x00")
	log := make([]byte, 16384)
	attr := make([]byte, 72)
	binary.LittleEndian.PutUint32(attr[0:], 15) // BPF_PROG_TYPE_CGROUP_DEVICE
	binary.LittleEndian.PutUint32(attr[4:], uint32(len(insns)))
	binary.LittleEndian.PutUint64(attr[8:], uint64(uintptr(unsafe.Pointer(&insns[0]))))
	binary.LittleEndian.PutUint64(attr[16:], uint64(uintptr(unsafe.Pointer(&license[0]))))
	binary.LittleEndian.PutUint32(attr[68:], 6) // expected BPF_CGROUP_DEVICE attach type
	binary.LittleEndian.PutUint32(attr[24:], 1)
	binary.LittleEndian.PutUint32(attr[28:], uint32(len(log)))
	binary.LittleEndian.PutUint64(attr[32:], uint64(uintptr(unsafe.Pointer(&log[0]))))
	fd, e := bpfCall(5, attr) // BPF_PROG_LOAD
	runtime.KeepAlive(insns)
	runtime.KeepAlive(license)
	runtime.KeepAlive(log)
	if e != nil {
		return fmt.Errorf("load required device filter: %w: %s", e, bytes.TrimRight(log, "\x00"))
	}
	defer syscall.Close(int(fd))
	attach := make([]byte, 20)
	binary.LittleEndian.PutUint32(attach[0:], uint32(group.Fd()))
	binary.LittleEndian.PutUint32(attach[4:], uint32(fd))
	binary.LittleEndian.PutUint32(attach[8:], 6)  // BPF_CGROUP_DEVICE
	binary.LittleEndian.PutUint32(attach[12:], 2) // ALLOW_MULTI: stacked filters AND together
	if _, e := bpfCall(8, attach); e != nil {
		return fmt.Errorf("attach required device filter: %w", e)
	}
	return nil
}
