// Command unreadable scans a directory and reports files that cannot be read
// (in use, no permission, path too long, I/O errors, etc.).
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
	"sync"
	"sync/atomic"
	"text/tabwriter"
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
		root        string
		deep        bool
		csvPath     string
		workers     int
		progressN   int
		showVersion bool
	)
	flag.StringVar(&root, "path", "", "directory to scan (required)")
	flag.BoolVar(&deep, "deep", false, "deep read: read each file's full contents to catch errors that only surface mid-read, e.g. bad sectors (more thorough but slower)")
	flag.StringVar(&csvPath, "csv", "", "optional: write the problem list to this CSV path")
	flag.IntVar(&workers, "workers", runtime.NumCPU(), "number of concurrent workers (default = CPU cores; lower for a single HDD to avoid head thrashing, higher for SSD/network shares)")
	flag.IntVar(&progressN, "progress", 500, "print progress every N files checked (0 to disable)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("unreadable %s\n", version)
		return
	}

	if root == "" {
		fmt.Fprintln(os.Stderr, "usage: unreadable -path <dir> [-deep] [-csv report.csv] [-workers N]")
		os.Exit(2)
	}
	if workers < 1 {
		workers = 1
	}
	if _, err := os.Lstat(root); err != nil {
		fmt.Fprintf(os.Stderr, "directory not found or inaccessible: %s (%v)\n", root, err)
		os.Exit(1)
	}

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

	// Producer: stream the directory tree, dispatching each file to jobs.
	go func() {
		defer close(jobs)
		defer close(producerDone)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
		})
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
				n := atomic.AddInt64(&processed, 1)
				if progressN > 0 && n%int64(progressN) == 0 {
					fmt.Fprintf(os.Stderr, "  checked %d files...\n", n)
				}
			}
		}()
	}

	// Close problems only after the producer and all workers have finished.
	go func() {
		<-producerDone
		wg.Wait()
		close(problems)
	}()

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

	total := atomic.LoadInt64(&processed)
	fmt.Printf("\nScanned %d files in: %s\n", total, root)
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
