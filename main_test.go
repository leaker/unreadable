package main

import (
	"os"
	"path/filepath"
	"testing"
)

// These cases run on every platform. On windows-latest they exercise the real
// syscall.CreateFile + FILE_SHARE_READ + longPath path in open_windows.go —
// the code that ships to Scoop users but can't be tested when developing on
// macOS/Linux.

func TestOpenForCheckReadable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ok.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := openForCheck(p); err != nil {
		t.Errorf("readable file should open, got error: %v", err)
	}
}

func TestOpenForCheckMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "does-not-exist")
	if err := openForCheck(p); err == nil {
		t.Error("a missing file should return an error, got nil")
	}
}
