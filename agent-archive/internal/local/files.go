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
)

func ID() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func Home() (string, error) {
	path := os.Getenv("AGENT_ARCHIVE_HOME")
	if path == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		path = filepath.Join(home, ".local/share/agent-archive")
	}
	path, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	// Resolve the deepest existing ancestor so symlinked paths cannot bypass the
	// Git-checkout exclusion before the final directory exists.
	ancestor := path
	for {
		if _, e = os.Stat(ancestor); e == nil {
			break
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return "", errors.New("invalid archive directory")
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
	path = filepath.Join(real, rel)
	for p := path; ; p = filepath.Dir(p) {
		if _, e = os.Stat(filepath.Join(p, ".git")); e == nil {
			return "", errors.New("archive storage must be outside Git checkouts")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if e = os.MkdirAll(path, 0700); e != nil {
		return "", e
	}
	if e = os.Chmod(path, 0700); e != nil {
		return "", e
	}
	return path, nil
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

func Lock(home string) (func(), error) {
	f, e := os.OpenFile(filepath.Join(home, "collector.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, ErrBusy
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
