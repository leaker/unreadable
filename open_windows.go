//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"syscall"
)

// openForCheck opens the file read-only while only allowing others to read it,
// faithfully reproducing the original PowerShell script's
// [System.IO.FileShare]::Read semantics:
//   - exclusively locked / being written by another process -> CreateFile fails
//     with a sharing violation
//   - no read permission -> ERROR_ACCESS_DENIED
//   - path too long -> longPath adds the \\?\ prefix to work around it; if it
//     still fails the error is reported faithfully
//
// We request GENERIC_READ so the check reflects real read access, but never
// read any bytes — a successful open is enough to call the file readable.
// Go's standard os.Open uses a more permissive share mode (Write/Delete) on
// Windows and would miss files that are "being written", so we go straight to
// syscall.CreateFile.
func openForCheck(path string) error {
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
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return err
	}
	syscall.CloseHandle(h)
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
