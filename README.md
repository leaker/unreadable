# unreadable

Scan a directory for **unreadable** files — ones that are in use, lack
permission, have a path that's too long, or hit I/O errors (bad sectors).

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

Grab `unreadable-*-windows-amd64.zip` from the
[Releases](https://github.com/leaker/unreadable/releases) page and extract
`unreadable.exe`.

## Usage

```
unreadable [options] <dir> [<dir>...]
```

One or more directories to scan are passed as positional arguments. Options
must come before the directories.

| Option | Description |
| --- | --- |
| `-deep` | Deep read: read each file's full contents to catch errors that only surface mid-read, e.g. bad sectors (slower, more disk I/O) |
| `-csv` | Optional: write the problem list to this CSV (UTF-8 BOM, Excel-friendly) |
| `-workers` | Concurrency, default = CPU cores. Lower it for a **single HDD** to avoid head thrashing; raise it for **SSD / network shares** |
| `-progress` | Live progress counter on stderr that updates in place; on by default, `-progress=false` to disable (auto-off when stderr isn't a terminal, so non-interactive runs print only the final result) |
| `-version` | Print version and exit |

Examples:

```powershell
unreadable.exe "D:\MyFolder"
unreadable.exe "D:\Photos" "E:\Backup"
unreadable.exe -deep -csv "C:\temp\unreadable.csv" "D:\MyFolder"
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

- `unreadable` — the file could be enumerated but failed to open (or to read,
  with `-deep`): in use, no permission, or an I/O error.
- `cannot enumerate` — the directory itself couldn't be entered, typically a
  no-permission subdirectory or a path that's too long.
