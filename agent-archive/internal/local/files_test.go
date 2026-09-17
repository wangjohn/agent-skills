package local

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateAtomicFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "state.json")
	if e := Write(p, map[string]int{"a": 1}); e != nil {
		t.Fatal(e)
	}
	var got map[string]int
	if e := Read(p, &got); e != nil {
		t.Fatal(e)
	}
	if got["a"] != 1 {
		t.Fatal(got)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}
func TestLockExcludesOtherCollector(t *testing.T) {
	home := t.TempDir()
	unlock, e := Lock(home)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Lock(home); e != ErrBusy {
		t.Fatal("second writer admitted", e)
	}
	unlock()
	unlock, e = Lock(home)
	if e != nil {
		t.Fatal(e)
	}
	unlock()
}
func TestHomeRejectsGitSymlink(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(repo, ".git"), 0700)
	os.Symlink(repo, filepath.Join(root, "link"))
	t.Setenv("AGENT_ARCHIVE_HOME", filepath.Join(root, "link", "private"))
	if _, e := Home(); e == nil {
		t.Fatal("allowed private data under Git via symlink")
	}
}
