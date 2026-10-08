package unitruntime

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/observe"
)

const UnitLogLimit = 1 << 20

// The independent host sink owns the files. A Unit only gets a pipe; daemon or
// monitor SIGKILL cannot remove the byte bound or expose host file descriptors.
func (m *Manager) openLogSink(id string) (*os.File, error) {
	path := filepath.Join(m.unitDir(id), "logs", "unit.log")
	input, output, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ready, signal, err := os.Pipe()
	if err != nil {
		input.Close()
		output.Close()
		return nil, err
	}
	cmd := exec.Command(m.cfg.InitBinary, "--unit-log-sink", path)
	cmd.Stdin = input
	cmd.ExtraFiles = []*os.File{signal}
	cmd.Stderr = os.Stderr
	cmd.Env = []string{"LANG=C"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		input.Close()
		output.Close()
		ready.Close()
		signal.Close()
		return nil, err
	}
	input.Close()
	signal.Close()
	defer ready.Close()
	if err = ready.SetReadDeadline(time.Now().Add(10 * time.Second)); err == nil {
		var value [1]byte
		_, err = io.ReadFull(ready, value[:])
		if err == nil && value[0] != 1 {
			err = fmt.Errorf("log sink did not initialize")
		}
	}
	if err != nil {
		output.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("bounded log startup: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return output, nil
}

// RunLogSink drains until every namespace/monitor pipe writer closes. On disk
// failure it keeps draining so logging cannot kill/block the workload. A small
// preallocated marker records degradation, without copying errors/payloads.
func RunLogSink(path string) error {
	if os.Geteuid() != 0 || !filepath.IsAbs(path) || filepath.Base(path) != "unit.log" {
		return fmt.Errorf("invalid privileged log sink")
	}
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe log directory")
	}
	fd, err := syscall.Open(filepath.Join(dir, "sink-status"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	status := os.NewFile(uintptr(fd), "log-status")
	defer status.Close()
	if i, e := status.Stat(); e != nil || !i.Mode().IsRegular() {
		return fmt.Errorf("unsafe sink status")
	}
	if _, err = status.WriteAt([]byte("0"), 0); err != nil {
		return err
	}
	if err = status.Sync(); err != nil {
		return err
	}
	if err = observe.Append(dir, "unit.log", nil, UnitLogLimit, false); err != nil {
		return err
	}
	ready := os.NewFile(3, "log-ready")
	syscall.CloseOnExec(3)
	if _, err = ready.Write([]byte{1}); err != nil {
		ready.Close()
		return err
	}
	ready.Close()
	buffer := make([]byte, 32<<10)
	failed := false
	for {
		n, e := os.Stdin.Read(buffer)
		if n > 0 && !failed {
			if err = observe.Append(dir, "unit.log", buffer[:n], UnitLogLimit, false); err != nil {
				failed = true
				_, _ = status.WriteAt([]byte("1"), 0)
				_ = status.Sync()
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
	}
	if failed {
		return fmt.Errorf("bounded log sink storage failed; output was drained")
	}
	return nil
}

// ReadLogs returns a bounded tail even for legacy logs created before the sink.
func (m *Manager) ReadLogs(id string) ([]byte, error) {
	path, err := m.LogsPath(id)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "unit-log-tail")
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("log is not regular")
	}
	if info.Size() > UnitLogLimit {
		if _, err = f.Seek(-UnitLogLimit, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(f, UnitLogLimit))
}

func (m *Manager) LogSinkFailed(id string) bool {
	if !objectName.MatchString(id) {
		return true
	}
	path := filepath.Join(m.unitDir(id), "logs", "sink-status")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	f := os.NewFile(uintptr(fd), "sink-status")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	var status [1]byte
	_, err = io.ReadFull(f, status[:])
	return err != nil || status[0] != '0'
}
