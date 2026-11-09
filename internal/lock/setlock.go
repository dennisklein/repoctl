package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
)

// Holder is the record of the process that holds a set lock.
type Holder struct {
	Command string    `json:"command"`
	PID     int       `json:"pid"`
	Host    string    `json:"host"`
	User    string    `json:"user"`
	Started time.Time `json:"started"`
	Run     string    `json:"run,omitempty"`
}

// SetLock is a held set lock, check lock or host mock lock: a flock(2) of a
// file that records the holder. It ends with its process.
type SetLock struct {
	path   string
	f      *os.File
	holder Holder
	shared bool
	ino    uint64
}

// What names the kind of lock in messages: "set slurm-26.05", "the host
// mock lock", "the check lock of slurm-26.05".
type What struct {
	// Code is the problem code of a busy lock: set.busy, mock.busy or
	// git.busy.
	Code api.ProblemCode
	// Name is the object of the message, e.g. "set slurm-26.05".
	Name string
	// Lock is the name of the lock, e.g. "the set lock".
	Lock string
	// Retry is the command of the wait hint, e.g. "repoctl build --lock-wait 2h".
	Retry string
}

// Acquire takes the lock at path. It waits up to wait (0: no wait) and
// then stops with a busy error that names the holder. shared takes a shared
// lock, which records no holder.
func Acquire(ctx context.Context, path string, h Holder, wait time.Duration, shared bool, what What) (*SetLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if shared {
		how = syscall.LOCK_SH
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if wait <= 0 || time.Now().After(deadline) {
			f.Close()
			return nil, BusyError(path, what)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	l := &SetLock{path: path, f: f, holder: h, shared: shared, ino: inode(f)}
	if !shared {
		if err := l.write(); err != nil {
			l.Release()
			return nil, err
		}
	}
	return l, nil
}

// BusyError gives the busy error of a held lock, with its holder.
func BusyError(path string, what What) error {
	h, _ := ReadHolder(path)
	by := "another process"
	if h != nil {
		by = "repoctl " + h.Command
		detail := fmt.Sprintf("pid %d on %s", h.PID, h.Host)
		if h.Run != "" {
			detail = "run " + h.Run + ", " + detail
		}
		by += " holds " + what.Lock + " (" + detail + ")"
	} else {
		by += " holds " + what.Lock
	}
	e := api.Errorf(what.Code, "%s is busy: %s", what.Name, by)
	if h != nil && h.Run != "" {
		e = e.WithRun(h.Run).WithHint("watch that run", "repoctl runs "+h.Run+" --follow")
	}
	if what.Retry != "" {
		e = e.WithHint("wait for it", what.Retry)
	}
	return e
}

func (l *SetLock) write() error {
	b, _ := json.Marshal(l.holder)
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	if _, err := l.f.WriteAt(append(b, '\n'), 0); err != nil {
		return err
	}
	return l.f.Sync()
}

// SetRun records the run id in the holder record.
func (l *SetLock) SetRun(run string) error {
	l.holder.Run = run
	if l.shared {
		return nil
	}
	return l.write()
}

// Holder gives the holder record of this lock.
func (l *SetLock) Holder() Holder { return l.holder }

// Lost reports that the lock path no longer names the locked file, for
// example after systemd removed $XDG_RUNTIME_DIR at logout.
func (l *SetLock) Lost() bool {
	fi, err := os.Stat(l.path)
	if err != nil {
		return true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return !ok || uint64(st.Ino) != l.ino //nolint:unconvert // the type differs per platform
}

// Retake takes the lock again at the same path after Lost. It fails when
// another process holds it now.
func (l *SetLock) Retake(ctx context.Context, what What) error {
	nl, err := Acquire(ctx, l.path, l.holder, 0, l.shared, what)
	if err != nil {
		return err
	}
	l.f.Close()
	*l = *nl
	return nil
}

// Release lets go of the lock. It can be called more than once.
func (l *SetLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	if !l.shared {
		_ = l.f.Truncate(0)
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
}

func inode(f *os.File) uint64 {
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino) //nolint:unconvert // the type differs per platform
	}
	return 0
}

// ReadHolder gives the holder of the lock at path, or nil when no process
// holds it.
func ReadHolder(path string) (*Holder, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	// A shared lock that succeeds means that no writer holds it.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return nil, nil
	}
	var h Holder
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return &Holder{}, nil
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return &Holder{}, nil
	}
	return &h, nil
}
