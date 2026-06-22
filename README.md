# unreadable

Scan a directory for **unreadable** files — ones that can't be opened for
reading: in use, no permission, path too long, or any other open error.

This is a Go rewrite of an earlier PowerShell script that fixed two problems:

| Problem | PowerShell version | Go version |
| --- | --- | --- |
| **Ran out of memory** | `Get-ChildItem -Recurse` materializes the whole tree into one big list | `filepath.WalkDir` streams the tree with a bounded channel for backpressure — constant memory |
| **Underused the CPU** | single-threaded, opening files one at a time; CPU idles during I/O waits | a worker pool checks files concurrently, default = CPU core count |

The "in use" check relies on Windows' `FileShare.Read` semantics. The Windows
build uses `syscall.CreateFile` + `FILE_SHARE_READ` (standard library only, no
third-party dependencies) to faithfully reproduce the original script; other
platforms fall back to `os.Open` for convenient development (but Unix has no
mandatory file locks, so "in use" isn't detectable there).

## Install

### Scoop (Windows, recommended)

```powershell
scoop bucket add leaker https://github.com/leaker/scoop-bucket
scoop install unreadable
```

Then `scoop update unreadable` upgrades it.

### Direct download

From the [Releases](https://github.com/leaker/unreadable/releases) page grab
`unreadable-*-windows-amd64.zip` (64-bit) or `unreadable-*-windows-386.zip`
(32-bit) and extract `unreadable.exe`. (Scoop picks the right one for you.)

## Usage

```
unreadable [options] <dir> [<dir>...]
```

One or more directories to scan are passed as positional arguments. Options
must come before the directories.

| Option | Description |
| --- | --- |
| `-csv` | Optional: write the problem list to this CSV (UTF-8 BOM, Excel-friendly) |
| `-workers` | Max concurrent file opens. Default `0` = **adaptive**: auto-tunes to the hardware by watching throughput (climbs on SSD / NVMe / network, backs off on a seek-bound HDD). Pass a fixed `N` to pin it and disable auto-tuning (e.g. `-workers 4` for a single HDD) |
| `-progress` | Live progress counter on stderr that updates in place; on by default, `-progress=false` to disable (auto-off when stderr isn't a terminal, so non-interactive runs print only the final result) |
| `-version` | Print version and exit |

Examples:

```powershell
unreadable.exe "D:\MyFolder"
unreadable.exe "D:\Photos" "E:\Backup"
unreadable.exe -csv "C:\temp\unreadable.csv" "D:\MyFolder"
```

## Build

Local build:

```bash
go build -o unreadable .
```

Cross-compile a Windows binary from macOS / Linux (no third-party dependencies):

```bash
GOOS=windows GOARCH=amd64 go build -o unreadable.exe .
```

## Output

- `unreadable` — the file could be enumerated but failed to open for reading:
  in use, no permission, path too long, or another open error.
- `cannot enumerate` — the directory itself couldn't be entered, typically a
  no-permission subdirectory or a path that's too long.

The progress line and final summary report throughput (`files/s`) and elapsed
time, so you can compare settings empirically.

## Performance

unreadable **opens every file** to test whether it can actually be read — that
is the whole point, and it is fundamentally different from size-only scanners
like WizTree, which read the NTFS MFT in bulk and never open a file. Whether a
file is locked or readable right now is a runtime condition that can't be read
from metadata, so a per-file open is unavoidable. That open — not CPU — is the
bottleneck.

Each open spends almost all its time *waiting* (filesystem + antivirus + disk),
not on the CPU — so the lever is concurrency (how many opens are in flight), not
CPU speed. The catch: the best concurrency depends on the hardware (an HDD wants
a few, an NVMe wants hundreds), which you can't know up front.

- **Concurrency auto-tunes by default (`-workers 0`).** A controller watches the
  live `files/s` throughput and hill-climbs the number of concurrent opens —
  raising it while throughput keeps rising, backing off when it stops. It
  converges to whatever the current disk / antivirus / network can sustain (high
  on SSD/NVMe/network, low on a seek-bound HDD) with no flags and no disk
  detection. The progress line shows the current concurrency as `…Nw)`.
- **Pin it with `-workers N` when you want determinism** — e.g. `-workers 4` on a
  known single HDD, or a high fixed value to benchmark. This disables
  auto-tuning.
- Concurrency is essentially free on RAM: a file is only opened, never read, so
  workers hold no buffers.
- **Exclude the target from antivirus.** Windows Defender's real-time
  protection scans the contents of every file you open; on a multi-million-file
  scan this is frequently the dominant cost. Adding the drive/folder to
  Defender exclusions (or testing with real-time protection off) often gives the
  biggest single speedup — bigger than any concurrency change.
