// Command unreadable scans one or more directories and reports files that
// cannot be read (in use, no permission, path too long, I/O errors, etc.).
//
// Two improvements over the original PowerShell version:
//   - Memory: filepath.WalkDir streams the tree, and a bounded channel feeding
//     a fixed worker pool applies backpressure — so it never materializes the
//     whole tree at once the way `Get-ChildItem -Recurse` does.
//   - Concurrency: a worker pool opens files in parallel to saturate multiple
//     cores / hide disk I/O latency, instead of checking files one at a time.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
)

// DeepRead reuses one 1MB buffer per worker to avoid re-allocating per file.
const bufSize = 1 << 20

// version is injected by the release pipeline via -ldflags "-X main.version=<tag>";
// local builds report "dev".
var version = "dev"

type problem struct {
	Path   string
	Status string
	Reason string
}

func main() {
	var (
		deep        bool
		csvPath     string
		workers     int
		showProg    bool
		showVersion bool
	)
	flag.BoolVar(&deep, "deep", false, "deep read: read each file's full contents to catch errors that only surface mid-read, e.g. bad sectors (more thorough but slower)")
	flag.StringVar(&csvPath, "csv", "", "optional: write the problem list to this CSV path")
	flag.IntVar(&workers, "workers", runtime.NumCPU(), "number of concurrent workers (default = CPU cores; lower for a single HDD to avoid head thrashing, higher for SSD/network shares)")
	flag.BoolVar(&showProg, "progress", true, "show a live progress counter on stderr (updates in place; auto-disabled when stderr is not a terminal)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: unreadable [options] <dir> [<dir>...]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Scan one or more directories for files that can't be read")
		fmt.Fprintln(os.Stderr, "(in use, no permission, path too long, I/O errors).")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options (must come before the directories):")
		flag.PrintDefaults()
	}
	flag.Parse()

	if showVersion {
		fmt.Printf("unreadable %s\n", version)
		return
	}

	roots := flag.Args()
	if len(roots) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if workers < 1 {
		workers = 1
	}

	// Validate the supplied paths; warn on bad ones but scan the rest.
	var valid []string
	for _, r := range roots {
		if _, err := os.Lstat(r); err != nil {
			fmt.Fprintf(os.Stderr, "skipping %s: %v\n", r, err)
			continue
		}
		valid = append(valid, r)
	}
	if len(valid) == 0 {
		fmt.Fprintln(os.Stderr, "no valid directory to scan")
		os.Exit(1)
	}
	roots = valid

	// Only animate the in-place counter on a real terminal; when stderr is
	// redirected (non-interactive), the \r repaints would just be noise in the
	// file — so there we print nothing and let the final summary stand alone.
	progress := showProg && isTerminal(os.Stderr)

	// Prepare the CSV before launching goroutines so a failure can exit cleanly.
	var (
		csvFile *os.File
		csvW    *csv.Writer
	)
	if csvPath != "" {
		f, err := os.Create(csvPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot create CSV: %v\n", err)
			os.Exit(1)
		}
		csvFile = f
		// UTF-8 BOM so Excel detects the encoding correctly.
		csvFile.Write([]byte{0xEF, 0xBB, 0xBF})
		csvW = csv.NewWriter(csvFile)
		csvW.Write([]string{"Path", "Status", "Reason"})
	}

	jobs := make(chan string, workers*2)
	problems := make(chan problem, 256)
	producerDone := make(chan struct{})
	var processed int64

	// Producer: stream each directory tree, dispatching every file to jobs.
	go func() {
		defer close(jobs)
		defer close(producerDone)
		walk := func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				// The directory itself can't be enumerated: no-permission
				// subdir, path too long, etc.
				problems <- problem{Path: p, Status: "cannot enumerate", Reason: err.Error()}
				if d != nil && d.IsDir() {
					return fs.SkipDir // can't enter this dir; skip it, keep walking siblings
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			// Only check regular files; skip symlinks and other irregular files
			// so we don't follow reparse points in circles.
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			jobs <- p // blocks when full — natural backpressure, so memory stays bounded
			return nil
		}
		for _, root := range roots {
			_ = filepath.WalkDir(root, walk)
		}
	}()

	// Worker pool: open/read files concurrently.
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []byte
			if deep {
				buf = make([]byte, bufSize)
			}
			for p := range jobs {
				if err := openForCheck(p, deep, buf); err != nil {
					problems <- problem{Path: p, Status: "unreadable", Reason: err.Error()}
				}
				atomic.AddInt64(&processed, 1)
			}
		}()
	}

	// Close problems only after the producer and all workers have finished.
	go func() {
		<-producerDone
		wg.Wait()
		close(problems)
	}()

	// Live progress: a ticker repaints a single in-place counter on stderr,
	// decoupled from how fast files are processed (no per-file write, no flicker).
	var progressDone, progressStopped chan struct{}
	if progress {
		progressDone = make(chan struct{})
		progressStopped = make(chan struct{})
		go func() {
			defer close(progressStopped)
			t := time.NewTicker(150 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					fmt.Fprintf(os.Stderr, "\r  checked %d files...", atomic.LoadInt64(&processed))
				case <-progressDone:
					// Final repaint (longer than any partial line, so it fully
					// overwrites) and commit it with a newline.
					fmt.Fprintf(os.Stderr, "\r  checked %d files... done\n", atomic.LoadInt64(&processed))
					return
				}
			}
		}()
	}

	// Collector: a single goroutine drains problems, writing the CSV
	// incrementally to keep memory low.
	var found []problem
	for pb := range problems {
		found = append(found, pb)
		if csvW != nil {
			csvW.Write([]string{pb.Path, pb.Status, pb.Reason})
		}
	}
	if csvW != nil {
		csvW.Flush()
		csvFile.Close()
	}

	// Stop the progress line before printing the summary so they don't interleave.
	if progress {
		close(progressDone)
		<-progressStopped
	}

	total := atomic.LoadInt64(&processed)
	fmt.Printf("\nScanned %d files in: %s\n", total, strings.Join(roots, ", "))
	if len(found) == 0 {
		fmt.Println("All files readable; no problems found.")
		return
	}

	fmt.Printf("Found %d problem(s):\n", len(found))
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "Path\tStatus\tReason")
	for _, pb := range found {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", pb.Path, pb.Status, pb.Reason)
	}
	tw.Flush()
	if csvPath != "" {
		fmt.Printf("Report written: %s\n", csvPath)
	}
}

// isTerminal reports whether f is a character device (an interactive terminal)
// rather than a regular file or pipe. Standard library only, works on Windows.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
