//go:build !windows

package main

import (
	"io"
	"os"
)

// openForCheck is the fallback for non-Windows platforms, handy for developing
// and testing on macOS/Linux.
//
// Note: Unix has no mandatory file locking like Windows, so almost any file can
// be opened and read. This mainly catches "no permission / missing / I/O error
// during DeepRead"; it cannot detect "in use" — that detection is
// Windows-specific, see open_windows.go.
func openForCheck(path string, deepRead bool, buf []byte) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	if deepRead {
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
