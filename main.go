// Command unreadable scans one or more directories and reports files that
// cannot be opened for reading (in use, no permission, path too long, etc.).
//
// Design notes:
//   - Memory: filepath.WalkDir streams the tree and a bounded jobs channel
//     applies backpressure, so it never materializes the whole tree the way
//     `Get-ChildItem -Recurse` does.
//   - Concurrency: opening a file is latency-bound (it waits on the filesystem /
//     antivirus / disk), so throughput comes from keeping many opens in flight.
//     By default an adaptive controller auto-tunes that concurrency to the
//     hardware by watching throughput — high on SSD/NVMe/network, low on a
//     seek-bound HDD — with no flags. A fixed -workers N disables it.
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
		csvPath     string
		workers     int
		showProg    bool
		showVersion bool
	)
	flag.StringVar(&csvPath, "csv", "", "optional: write the problem list to this CSV path")
	flag.IntVar(&workers, "workers", 0, "max concurrent file opens; 0 = adaptive: auto-tune to the hardware by watching throughput (default). Pass a fixed N to disable auto-tuning, e.g. -workers 4 for a single HDD")
	flag.BoolVar(&showProg, "progress", true, "show a live progress counter on stderr (updates in place; auto-disabled when stderr is not a terminal)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: unreadable [options] <dir> [<dir>...]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Scan one or more directories for files that can't be opened for reading")
		fmt.Fprintln(os.Stderr, "(in use, no permission, path too long).")
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
	if workers < 0 {
		workers = 0
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
	// redirected (non-interactive), the \r repaints would just be noise — there
	// we print nothing and let the final summary stand alone.
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
		csvFile.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM for Excel
		csvW = csv.NewWriter(csvFile)
		csvW.Write([]string{"Path", "Status", "Reason"})
	}

	start := time.Now()
	jobs := make(chan string, 1024)
	problems := make(chan problem, 256)
	producerDone := make(chan struct{})
	var processed int64
	var curConc int64 // current open concurrency, for the progress display

	// Concurrency cap: adaptive by default, fixed when -workers N is given.
	// `limit` (inside sm) is the live cap on concurrent opens; the pool holds
	// maxConc goroutines but only `limit` of them open a file at once.
	adaptive := workers == 0
	initLimit, maxConc := workers, workers
	if adaptive {
		initLimit = runtime.NumCPU()
		maxConc = runtime.NumCPU() * 16
		if maxConc < 256 {
			maxConc = 256
		}
	}
	sm := newSem(initLimit)
	atomic.StoreInt64(&curConc, int64(initLimit))

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
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil // skip symlinks; don't follow reparse points in circles
			}
			jobs <- p
			return nil
		}
		for _, root := range roots {
			_ = filepath.WalkDir(root, walk)
		}
	}()

	// Worker pool: maxConc goroutines, but the semaphore caps how many opens
	// run at once — that cap is what the controller tunes.
	var wg sync.WaitGroup
	for i := 0; i < maxConc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				sm.acquire()
				err := openForCheck(p)
				sm.release()
				if err != nil {
					problems <- problem{Path: p, Status: "unreadable", Reason: err.Error()}
				}
				atomic.AddInt64(&processed, 1)
			}
		}()
	}

	// Close problems once every worker has exited (jobs drained).
	go func() {
		wg.Wait()
		close(problems)
	}()

	// Adaptive controller: measure throughput across a ladder of concurrency
	// levels and hold the fastest (see autotune). Stops when enumeration is done.
	if adaptive {
		go autotune(sm, &processed, &curConc, maxConc, producerDone)
	}

	// Live progress: a ticker repaints a single in-place line with throughput
	// and the current concurrency, decoupled from how fast files are processed.
	var progressDone, progressStopped chan struct{}
	if progress {
		progressDone = make(chan struct{})
		progressStopped = make(chan struct{})
		go func() {
			defer close(progressStopped)
			prevLen := 0
			emit := func(suffix string) {
				n := atomic.LoadInt64(&processed)
				line := fmt.Sprintf("  checked %d files (%s, %dw)...%s", n, rate(n, start), atomic.LoadInt64(&curConc), suffix)
				if pad := prevLen - len(line); pad > 0 {
					line += strings.Repeat(" ", pad)
				}
				prevLen = len(line)
				fmt.Fprintf(os.Stderr, "\r%s", line)
			}
			t := time.NewTicker(150 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					emit("")
				case <-progressDone:
					emit(" done")
					fmt.Fprint(os.Stderr, "\n")
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

	if progress {
		close(progressDone)
		<-progressStopped
	}

	total := atomic.LoadInt64(&processed)
	elapsed := time.Since(start)
	fmt.Printf("\nScanned %d files in: %s\n%.1fs elapsed, %s (%dw)\n",
		total, strings.Join(roots, ", "), elapsed.Seconds(), rate(total, start), atomic.LoadInt64(&curConc))
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

// sem is a counting semaphore whose limit can change at runtime; the adaptive
// controller uses it to vary how many file opens run concurrently.
type sem struct {
	mu     sync.Mutex
	cond   *sync.Cond
	active int
	limit  int
}

func newSem(limit int) *sem {
	s := &sem{limit: limit}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *sem) acquire() {
	s.mu.Lock()
	for s.active >= s.limit {
		s.cond.Wait()
	}
	s.active++
	s.mu.Unlock()
}

func (s *sem) release() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	s.cond.Signal()
}

func (s *sem) setLimit(n int) {
	s.mu.Lock()
	prev := s.limit
	s.limit = n
	s.mu.Unlock()
	if n > prev {
		s.cond.Broadcast() // wake parked workers so they can run
	}
}

// autotune picks the open concurrency that maximizes throughput on the current
// hardware. The right value can't be known up front — an HDD wants a few opens
// in flight, an NVMe or network share wants hundreds — so it measures: it walks
// a ladder of concurrency levels, holds each one briefly while sampling files/s,
// and settles on the fastest. Comparing absolute throughput at held levels (not
// tick-to-tick deltas) means a transient antivirus/cache blip can't confound the
// decision, and it always leaves the limit at the best *measured* value, never a
// mid-probe one. It re-measures occasionally in case conditions change, and
// stops when enumeration finishes (the buffered tail then drains at that best).
func autotune(sm *sem, processed, curConc *int64, maxConc int, done <-chan struct{}) {
	// Concurrency ladder: 1 (HDD-friendly) up through maxConc (NVMe/network).
	var ladder []int
	for c := 1; c < maxConc; c *= 2 {
		ladder = append(ladder, c)
	}
	ladder = append(ladder, maxConc)

	const settle = 250 * time.Millisecond // let the new limit take effect
	const window = 400 * time.Millisecond // then sample throughput
	const reMeasure = 5 * time.Minute     // periodically re-check for drift

	// wait sleeps for d, returning false if the scan ended meanwhile.
	wait := func(d time.Duration) bool {
		select {
		case <-done:
			return false
		case <-time.After(d):
			return true
		}
	}
	set := func(n int) {
		atomic.StoreInt64(curConc, int64(n))
		sm.setLimit(n)
	}
	// sample measures files/s at concurrency c; ok=false if the scan ended.
	sample := func(c int) (float64, bool) {
		set(c)
		if !wait(settle) {
			return 0, false
		}
		before := atomic.LoadInt64(processed)
		if !wait(window) {
			return 0, false
		}
		after := atomic.LoadInt64(processed)
		return float64(after-before) / window.Seconds(), true
	}

	// committed is the last fully-validated winner; we fall back to it if the
	// scan ends mid-pass, so an in-progress re-measurement (which starts at the
	// bottom of the ladder) can never strand us on a transient low value.
	committed := int(atomic.LoadInt64(curConc))
	for {
		roundBest, bestT := ladder[0], -1.0
		for _, c := range ladder {
			t, ok := sample(c)
			if !ok {
				set(committed) // scan ending — settle on the last validated best
				return
			}
			if t > bestT {
				bestT, roundBest = t, c
			}
		}
		committed = roundBest
		set(committed) // exploit the winner
		if !wait(reMeasure) {
			return
		}
	}
}

// rate formats the average throughput since start as "<n>/s".
func rate(n int64, start time.Time) string {
	s := time.Since(start).Seconds()
	if s <= 0 {
		return "—/s"
	}
	return fmt.Sprintf("%.0f/s", float64(n)/s)
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
