package unitruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/security"
)

type probeRequest struct {
	State State `json:"state"`
	Probe Probe `json:"probe"`
}

// EnterProbeNamespace runs only in a separate init helper process. A locked
// thread enters the pinned network namespace, drops privileges and re-execs so
// every worker thread inherits that namespace and restricted credentials.
func EnterProbeNamespace(encoded string) error {
	var req probeRequest
	if err := json.Unmarshal([]byte(encoded), &req); err != nil {
		return err
	}
	if !processMatches(req.State) {
		return fmt.Errorf("Unit process identity changed")
	}
	namespace, err := os.Open(fmt.Sprintf("/proc/%d/ns/net", req.State.PID))
	if err != nil {
		return err
	}
	defer namespace.Close()
	if !processMatches(req.State) {
		return fmt.Errorf("Unit process identity changed")
	}
	executable, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer executable.Close()
	runtime.LockOSThread()
	nr := uintptr(308)
	if runtime.GOARCH == "arm64" {
		nr = 268
	} else if runtime.GOARCH != "amd64" {
		return fmt.Errorf("unsupported probe architecture")
	}
	if _, _, errno := syscall.RawSyscall(nr, namespace.Fd(), syscall.CLONE_NEWNET, 0); errno != 0 {
		return errno
	}
	policy := security.Policy{RunAsUID: 65534, RunAsGID: 65534}
	policy.Normalize()
	if err := security.Apply(policy); err != nil {
		return err
	}
	return syscall.Exec(fmt.Sprintf("/proc/self/fd/%d", executable.Fd()), []string{"titanus-init", "--probe-worker", encoded}, []string{"PATH=/usr/bin:/bin"})
}

func ProbeWorker(encoded string) error {
	var req probeRequest
	if err := json.Unmarshal([]byte(encoded), &req); err != nil {
		return err
	}
	h := Health{Readiness: &req.Probe}
	h.Normalize("never")
	if err := h.Validate(); err != nil {
		return err
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(req.Probe.Port))
	timeout := time.Duration(req.Probe.TimeoutSeconds) * time.Second
	if req.Probe.Protocol == "tcp" {
		conn, err := net.DialTimeout("tcp", address, timeout)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: 8192}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get("http://" + address + req.Probe.Path)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return fmt.Errorf("HTTP probe returned %d", response.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return nil
}

func (m *Manager) runProbe(ctx context.Context, state State, probe Probe) error {
	encoded, err := json.Marshal(probeRequest{State: state, Probe: probe})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(probe.TimeoutSeconds+2)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.cfg.InitBinary, "--probe-net", string(encoded))
	if output, err := cmd.CombinedOutput(); err != nil {
		if len(output) > 1024 {
			output = output[:1024]
		}
		return fmt.Errorf("probe failed: %s (%v)", output, err)
	}
	return nil
}
