//go:build !windows

package main

import "os"

// openForCheck is the fallback for non-Windows platforms, handy for developing
// and testing on macOS/Linux.
//
// Note: Unix has no mandatory file locking like Windows, so almost any file can
// be opened. This mainly catches "no permission / missing"; it cannot detect
// "in use" — that detection is Windows-specific, see open_windows.go.
func openForCheck(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	f.Close()
	return nil
}
