package keycard

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestAtomicWriteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uids.txt")
	for _, lines := range [][]string{{master1, user1}, {user2}, {}} {
		if err := atomicWriteLines(path, lines); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		for _, line := range lines {
			want += line + "\n"
		}
		if string(got) != want {
			t.Fatalf("contents = %q, want %q", got, want)
		}
		if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary file remains: %v", err)
		}
	}
}

func TestAtomicWriteLinesDirectorySyncFailure(t *testing.T) {
	testDirectorySyncFailure(t, func(path string) error {
		return atomicWriteLines(path, []string{user1})
	})
}

func TestAliasesDirectorySyncFailure(t *testing.T) {
	testDirectorySyncFailure(t, func(path string) error {
		a := &keyAliases{path: path, names: map[string]string{"card:" + user1: "Home"}}
		return a.save()
	})
}

func testDirectorySyncFailure(t *testing.T, write func(string) error) {
	t.Helper()
	if os.Getenv("KEYCARD_TEST_DIRSYNC") == "1" {
		// Keep both fsync calls on one thread for strace's per-thread counter.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := write(filepath.Join(t.TempDir(), "keys"))
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("directory fsync failure = %v, want EIO", err)
		}
		return
	}
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Skip("strace required for directory-fsync fault injection")
	}
	cmd := exec.Command(strace, "-f", "-qq", "-o", filepath.Join(t.TempDir(), "trace"),
		"-e", "inject=fsync:error=EIO:when=2", os.Args[0],
		"-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "KEYCARD_TEST_DIRSYNC=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("directory-fsync regression: %v\n%s", err, output)
	}
}
