// Package local provides private durable files and process coordination.
package local

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func ID() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func Home() (string, error)     { return resolveHome(true) }
func ReadHome() (string, error) { return resolveHome(false) }
func resolveHome(create bool) (string, error) {
	path := os.Getenv("AGENT_ARCHIVE_HOME")
	if path == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		path = filepath.Join(home, ".local/share/agent-archive")
	}
	path, e := ResolveExistingSymlinks(path)
	if e != nil {
		return "", errors.New("invalid archive directory")
	}
	for p := path; ; p = filepath.Dir(p) {
		if _, e = os.Stat(filepath.Join(p, ".git")); e == nil {
			return "", errors.New("archive storage must be outside Git checkouts")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if !create {
		return path, nil
	}
	if e = os.MkdirAll(path, 0700); e != nil {
		return "", e
	}
	if e = os.Chmod(path, 0700); e != nil {
		return "", e
	}
	return path, nil
}

// ResolveExistingSymlinks returns path made absolute with symlinks resolved
// through its deepest existing ancestor, so a path that does not exist yet
// still lands where it will really be created. Paths compared lexically
// elsewhere (project roots, the archive directory) must go through this
// first, or a symlinked spelling never matches the real one.
func ResolveExistingSymlinks(path string) (string, error) {
	path, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	ancestor := path
	for {
		if _, e = os.Stat(ancestor); e == nil {
			break
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return "", errors.New("no existing ancestor for " + path)
		}
		ancestor = next
	}
	real, e := filepath.EvalSymlinks(ancestor)
	if e != nil {
		return "", e
	}
	rel, e := filepath.Rel(ancestor, path)
	if e != nil {
		return "", e
	}
	return filepath.Join(real, rel), nil
}

func Write(path string, value any) error {
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	return WriteBytes(path, append(b, '\n'))
}
func WriteBytes(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".pending-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func Read(path string, value any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, value)
}

var ErrBusy = errors.New("another collector or setup is running")

func Lock(home string) (func(), error) { return NamedLock(home, "collector.lock") }

func NamedLock(home, name string) (func(), error) {
	f, e := os.OpenFile(filepath.Join(home, name), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		if errors.Is(e, syscall.EWOULDBLOCK) || errors.Is(e, syscall.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, e
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// NamedLockWait tolerates short contention while preserving the hook deadline.
func NamedLockWait(home, name string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		unlock, err := NamedLock(home, name)
		if !errors.Is(err, ErrBusy) {
			return unlock, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, err
		}
		time.Sleep(min(10*time.Millisecond, remaining))
	}
}
