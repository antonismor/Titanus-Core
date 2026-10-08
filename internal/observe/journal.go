package observe

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const JournalLimit = 1 << 20

// Events intentionally accept no arbitrary messages, bodies or maps. Secret
// material cannot enter the journal through error strings or request payloads.
type Event struct {
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	Request    string    `json:"request,omitempty"`
	Actor      string    `json:"actor,omitempty"`
	Role       string    `json:"role,omitempty"`
	Operation  string    `json:"operation,omitempty"`
	Method     string    `json:"method,omitempty"`
	TargetHash string    `json:"target_hash,omitempty"`
	Status     int       `json:"status,omitempty"`
	Object     string    `json:"object,omitempty"`
	State      string    `json:"state,omitempty"`
	Ready      bool      `json:"ready,omitempty"`
	Live       bool      `json:"live,omitempty"`
	Restarts   int       `json:"restarts,omitempty"`
}

type Recorder struct {
	Dir      string
	mu       sync.Mutex
	failures uint64
	requests map[RequestKey]RequestMetric
}

func New(stateRoot string) (*Recorder, error) {
	dir := filepath.Join(stateRoot, "observability")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("unsafe observability directory")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	r := &Recorder{Dir: dir, requests: map[RequestKey]RequestMetric{}}
	if err = r.Record(Event{Kind: "daemon.open"}); err != nil {
		return nil, err
	}
	return r, nil
}

func NewRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func TargetHash(path string) string {
	value := sha256.Sum256([]byte(path))
	return hex.EncodeToString(value[:])
}

func (r *Recorder) Record(e Event) error {
	e.Time = time.Now().UTC()
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err = Append(r.Dir, "events.ndjson", append(data, '\n'), JournalLimit, true); err != nil {
		r.failures++
	}
	return err
}

func (r *Recorder) Failures() uint64 { r.mu.Lock(); defer r.mu.Unlock(); return r.failures }

// Separate processes serialize append/rotation on a stable lock inode.
// Rotation never follows a symlink, and audit publication is fsynced.
func Append(dir, name string, data []byte, limit int64, durable bool) error {
	if filepath.Base(name) != name || limit <= 0 || int64(len(data)) > limit {
		return fmt.Errorf("invalid bounded log write")
	}
	lock, err := openRegular(filepath.Join(dir, name+".lock"), syscall.O_CREAT|syscall.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(dir, name)
	f, err := openRegular(path, syscall.O_CREAT|syscall.O_WRONLY|syscall.O_APPEND, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if info.Size()+int64(len(data)) > limit {
		if info.Size() > limit {
			// A pre-upgrade direct log may exceed the bound. Retain only its
			// last limit bytes before publishing it as the previous segment.
			old, e := openRegular(path, syscall.O_RDONLY, 0)
			if e != nil {
				f.Close()
				return e
			}
			if _, e = old.Seek(-limit, io.SeekEnd); e != nil {
				old.Close()
				f.Close()
				return e
			}
			tail, e := io.ReadAll(io.LimitReader(old, limit))
			old.Close()
			if e != nil {
				f.Close()
				return e
			}
			if e = f.Truncate(0); e != nil {
				f.Close()
				return e
			}
			if _, e = f.Write(tail); e != nil {
				f.Close()
				return e
			}
		}
		f.Close()
		previous := path + ".1"
		if i, e := os.Lstat(previous); e == nil && !i.Mode().IsRegular() {
			return fmt.Errorf("unsafe previous log")
		} else if e != nil && !os.IsNotExist(e) {
			return e
		}
		if err = os.Rename(path, previous); err != nil {
			return err
		}
		f, err = openRegular(path, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY, 0600)
		if err != nil {
			return err
		}
	}
	defer f.Close()
	if n, e := f.Write(data); e != nil {
		return e
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	if durable {
		if err = f.Sync(); err != nil {
			return err
		}
		d, e := os.Open(dir)
		if e != nil {
			return e
		}
		defer d.Close()
		return d.Sync()
	}
	return nil
}

func openRegular(path string, flags int, mode uint32) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, mode)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("log must be a regular file")
	}
	return f, nil
}

// Tail returns bounded retained events; it never reads workload logs/specs.
func (r *Recorder) Tail() ([]Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, err := openRegular(filepath.Join(r.Dir, "events.ndjson.lock"), syscall.O_CREAT|syscall.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	result := []Event{}
	for _, name := range []string{"events.ndjson.1", "events.ndjson"} {
		f, err := openRegular(filepath.Join(r.Dir, name), syscall.O_RDONLY, 0)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(io.LimitReader(f, JournalLimit+1))
		for {
			var e Event
			err = decoder.Decode(&e)
			if err == io.EOF {
				break
			}
			if err != nil {
				f.Close()
				return nil, err
			}
			result = append(result, e)
		}
		f.Close()
	}
	if len(result) > 256 {
		result = result[len(result)-256:]
	}
	return result, nil
}
