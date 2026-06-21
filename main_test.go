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
	if err := openForCheck(p, false, nil); err != nil {
		t.Errorf("readable file should pass quick mode, got error: %v", err)
	}
}

func TestOpenForCheckDeepReadMultiChunk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(p, make([]byte, 200*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64*1024) // small buffer to force several Read iterations
	if err := openForCheck(p, true, buf); err != nil {
		t.Errorf("deep read should succeed, got error: %v", err)
	}
}

func TestOpenForCheckMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "does-not-exist")
	if err := openForCheck(p, false, nil); err == nil {
		t.Error("a missing file should return an error, got nil")
	}
}
