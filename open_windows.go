//go:build windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// FILE_FLAG_SEQUENTIAL_SCAN hints the cache manager and speeds up DeepRead's
// sequential reads. syscall doesn't export it, so define it here.
const fileFlagSequentialScan = 0x08000000

// openForCheck opens the file read-only while only allowing others to read it,
// faithfully reproducing the original PowerShell script's
// [System.IO.FileShare]::Read semantics:
//   - exclusively locked by another process -> CreateFile fails with ERROR_SHARING_VIOLATION
//   - currently being written by another process -> the share mode excludes
//     Write, so it conflicts too
//   - no read permission -> ERROR_ACCESS_DENIED
//   - path too long -> longPath adds the \\?\ prefix to work around it; if it
//     still fails the error is reported faithfully
//
// Go's standard os.Open uses a more permissive share mode (Write/Delete) on
// Windows and would miss files that are "being written", so we go straight to
// syscall.CreateFile.
func openForCheck(path string, deepRead bool, buf []byte) error {
	p, err := syscall.UTF16PtrFromString(longPath(path))
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|fileFlagSequentialScan,
		0,
	)
	if err != nil {
		return err
	}
	// Hand the handle to os.File; its Close calls CloseHandle, so there's no
	// separate release to do.
	f := os.NewFile(uintptr(h), path)
	defer f.Close()

	if deepRead {
		// Read it through to catch I/O errors that only surface mid-read, e.g.
		// bad sectors.
		for {
			n, rerr := f.Read(buf)
			if rerr != nil {
				if rerr == io.EOF {
					break
				}
				return rerr
			}
			if n == 0 {
				break
			}
		}
	}
	return nil
}

// longPath prefixes an absolute path with \\?\ to bypass the 260-char MAX_PATH
// limit (supports up to ~32767 characters).
func longPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) {
		return p
	}
	abs, err := filepath.Abs(p) // also cleans . / .. and normalizes to backslashes
	if err != nil {
		abs = p
	}
	if strings.HasPrefix(abs, `\\`) {
		// UNC path: \\server\share -> \\?\UNC\server\share
		return `\\?\UNC` + abs[1:]
	}
	return `\\?\` + abs
}
