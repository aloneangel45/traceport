# TracePort

<p align="center">
  <img src="https://raw.githubusercontent.com/aloneangel45/traceport/main/assets/banner.png" alt="TracePort Banner" width="700" onerror="this.style.display='none'"/>
</p>

<p align="center">
  <strong>Deterministic Flight Recorder, Zstandard Shadow-Tree Engine & Time-Travel Debugger for AI Agents and CLI Workflows.</strong><br>
  Capture continuous multi-step agent executions, inspect filesystem diffs across steps, and roll back workspace state in milliseconds with zero LLM token overhead.
</p>

<p align="center">
  <a href="https://github.com/aloneangel45/traceport/actions/workflows/ci.yml"><img src="https://github.com/aloneangel45/traceport/actions/workflows/ci.yml/badge.svg" alt="CI Status"/></a>
  <a href="https://github.com/aloneangel45/traceport/releases"><img src="https://img.shields.io/github/v/release/aloneangel45/traceport?style=flat-square&color=blue" alt="Release"/></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go" alt="Go Version"/></a>
  <a href="https://opensource.org/licenses/MIT"><img src="https://img.shields.io/badge/License-MIT-blue.svg?style=flat-square" alt="License: MIT"/></a>
  <a href="#"><img src="https://img.shields.io/badge/Platform-Windows%20%7C%20Linux%20%7C%20macOS-blue?style=flat-square" alt="Cross-Platform"/></a>
  <a href="#"><img src="https://img.shields.io/badge/Snapshots-Zstd%20Shadow%20Trees-007ec6?style=flat-square" alt="Zstd Shadow Trees"/></a>
</p>

```text
$ traceport record -- aider --model claude-sonnet-4-5
[traceport] recording command: aider --model claude-sonnet-4-5
  step 01  checkpoint   Initial workspace snapshot [tree:84c1e9a04b12]
  step 08  checkpoint   Intermediate workspace snapshot [tree:2f91b7d4c083]
  step 14  error        syntax error in pkg/auth/middleware.go
[err] session trace_1794348120984102 aborted (exit=1, frames=15)

$ traceport diff trace_1794348120984102 --from 8 --to 15
--- a/pkg/auth/middleware.go
+++ b/pkg/auth/middleware.go
...

$ traceport rewind trace_1794348120984102 --step 8
[ok] pre-rewind safety savepoint created: 91e04ad8721b553f
[ok] rewound session trace_1794348120984102 to step 8 (restored=2, deleted=1, skipped=31)
```

---

## Overview

Autonomous coding agents execute multi-step workflows that mutate dozens of files across consecutive tool calls. When an agent introduces a regression or crashes mid-run, restarting from scratch wastes inference tokens, wall-clock time, and clean intermediate state.

TracePort wraps any CLI or agent process in a pure-Go (`CGO_ENABLED=0`) flight recorder:

- **Zstandard Content-Addressed Shadow Trees:** Captures initial, intermediate, and final workspace snapshots in any directory (Git or non-Git) without mutating `.git/index` or polluting commit history.
- **Dictionary-Accelerated Small-File Compression:** Ships with a deterministic multi-language Zstandard dictionary (`DefaultSourceDict`) for zero-cold-start compression on source files, plus `traceport dict train` for repository-specific `.zdict` training.
- **Incremental Stat Cache:** Indexes `(mtime, size, mode) -> sha256` so unchanged files incur zero disk reads and zero re-compression during high-frequency polling (~0.56 ms scan across 32 files).
- **Safe Delta Time-Travel Rewind:** Automatically records a pre-rewind safety savepoint before modifying the workspace, supports `--dry-run` previews, guards against cross-repository rewinds, skips unchanged files on disk, and removes post-checkpoint stray files while preserving `.git` and `.traceport`.
- **Append-Only JSONL Frame Log:** Streams arbitrarily large stdout/stderr payloads (>1 MiB single lines) in `O(1)` append operations (`<id>.frames.jsonl`) alongside atomic metadata (`<id>.meta.json`).
- **Session Tagging, Cross-Session Search & Redacted Exports:** Tag flights at record time or post-hoc (`--tag`, `traceport tag`), search across commands, stderr/stdout frames, tool calls, and checkpoint file mutations (`traceport search`), and export portable JSON fixtures or Markdown audit reports with built-in credential scrubbing (`traceport export --redact`).
- **Cryptographic Integrity & Garbage Collection:** Built-in `verify` (SHA-256 manifest + blob verification), `diff` (pure-Go unified diff), and `gc` (session pruning and unreferenced blob sweeping).

---

## Architecture

```text
+------------------------------------------------------------------------+
|                           traceport CLI                                |
|  record | sessions | tag | search | export | diff | rewind | verify    |
+-------------------+--------------------------------+-------------------+
                    |                                |
                    v                                v
        +-----------------------+        +-----------------------+
        |    Flight Recorder    |        |   Time-Travel Engine  |
        |  Chunked Pipe Stream  |        |  Delta Restore / Diff |
        |  Signal Forwarding    |        |  Verify / Blob GC     |
        +-----------+-----------+        +-----------+-----------+
                    |                                |
        +-----------+------------+-------------------+-----------+
        |                        |                               |
        v                        v                               v
+---------------+    +-----------------------+    +----------------------+
| Session Store |    | Zstd Shadow-Tree CAS  |    | Dictionary Registry  |
| .meta.json    |    | blobs/<xx>/<sha>.zst  |    | DefaultSourceDict +  |
| .frames.jsonl |    | trees/<hash>.manifest |    | dicts/dict_*.zdict   |
+---------------+    +-----------------------+    +----------------------+
```

### Storage Layout

TracePort resolves its store directory in order: `TRACEPORT_HOME` environment variable, nearest workspace `.traceport/` or `.git` root (`<repo>/.traceport`), or the OS user configuration directory.

```text
.traceport/
  sessions/
    trace_<id>.meta.json          # Atomic session metadata (0600)
    trace_<id>.frames.jsonl       # O(1) append-only telemetry stream (0600)
  blobs/
    <sha[:2]>/<sha[2:]>.zst       # Deduplicated Zstd-compressed file blobs (0600)
  trees/
    <treeHash>.manifest.zst       # Content-addressed Zstd tree manifests (0600)
    stat_cache.json               # Incremental (mtime, size, mode) -> sha256 index
  dicts/
    dict_<id>.zdict               # Trained workspace Zstandard dictionaries (0600)
    active                        # Active encoder dictionary pointer
```

---

## Benchmarks

Measured via `CGO_ENABLED=0 go test -bench=. -benchmem ./pkg/checkpoint/...` on AMD Ryzen 9 8945HX (`windows/amd64`, Go 1.27, NTFS) over a 5-step multi-language workspace trajectory (32 source files across Go, Python, TypeScript, and YAML; ~77.9 KB cumulative logical snapshot payload) followed by a delta rollback to step 2:

| Storage Backend | Stored Bytes / 5 Steps | Space Ratio (`ratio_pct`) | Delta Rollback (`rollback_us/op`) | Notes |
| :--- | ---: | ---: | ---: | :--- |
| `PureGoSQLiteWAL` | `193,672 B` | `248.6%` | `8,343 us` | 4 KiB WAL page framing + CRC32 headers |
| `Uncompressed` (CAS) | `40,540 B` | `52.04%` | `2,225 us` | SHA-256 deduplicated raw blobs + raw JSON manifests |
| `ZstdRaw` (`SpeedFastest`) | `20,008 B` | `25.68%` | `2,497 us` | Deduplicated Zstd blobs, no dictionary |
| **`ZstdDict` (`SpeedFastest`)** | **`10,846 B`** | **`13.92%`** | **`2,828 us`** | **Deduplicated Zstd + trained workspace `.zdict` (17.8x smaller than paged WAL)** |

Additional microbenchmarks on the same host:
- **Unchanged 32-file workspace checkpoint (`BenchmarkIncrementalStatCache_Unchanged`):** `557 us/op` (`0` file reads, `0` blob compressions).
- **Small Go file compression (`BenchmarkCompressorSmallSource`):** `ZstdDict` achieves `69.9%` ratio vs `77.3%` for raw `ZstdRaw` on sub-kilobyte snippets.

---

## Installation

### 1. Verified One-Line Install

Downloads the platform archive and `checksums.txt`, verifies the SHA-256 digest before extraction, and installs `traceport`:

```bash
# Linux & macOS (amd64 / arm64)
curl -fsSL https://raw.githubusercontent.com/aloneangel45/traceport/main/install.sh | bash
```

```powershell
# Windows PowerShell (amd64 / arm64)
irm https://raw.githubusercontent.com/aloneangel45/traceport/main/install.ps1 | iex
```

### 2. Pre-Compiled Release Archives

Download release binaries and `checksums.txt` from [GitHub Releases](https://github.com/aloneangel45/traceport/releases) (`windows/amd64`, `windows/arm64`, `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`).

### 3. Build from Source (`Go 1.25+`)

```bash
go install github.com/aloneangel45/traceport/cmd/traceport@latest
```

Or build locally without CGo:

```bash
git clone https://github.com/aloneangel45/traceport.git
cd traceport
CGO_ENABLED=0 go build -trimpath -o traceport ./cmd/traceport
```

---

## CLI Reference

### Record an Execution Session

```bash
traceport record -- claude code "Refactor authentication middleware"
traceport record --tag bug-42 --tag ci -- go test ./...
```

Child exit codes and `SIGINT`/`SIGTERM` termination states are preserved and propagated to the caller.

### Tag, List & Inspect Sessions

```bash
# List all sessions or filter by tag
traceport sessions
traceport list --tag bug-42
traceport list --tag bug-42 --json

# Add or remove tags on an existing session
traceport tag <session_id> regression hotfix
traceport tag <session_id> hotfix --remove --json

# Inspect session metadata and frame timeline
traceport inspect <session_id>
traceport inspect <session_id> --json
```

### Cross-Session Search

Scan recorded sessions across commands, tags, stdout/stderr frames, tool invocations, and shadow-tree checkpoint file mutations (`added`, `modified`, `deleted`):

```bash
# Case-insensitive substring search across all sessions (locates touched files or errors)
traceport search middleware.go
traceport search "syntax error" --tag bug-42

# Regular expression search with structured JSON output
traceport search "panic:|E_[A-Z_]+" --regex --json
```

### Portable Session Export & Secret Redaction

Export a self-contained monolithic JSON `model.Session` (compatible out-of-the-box with `agentfixture import -traceport <file>` and CI artifacts) or a structured Markdown flight audit report, optionally scrubbing API keys (`sk-*`, `sk-ant-*`, `ghp_*`, `github_pat_*`, `AIza*`, `AKIA*`, `hf_*`, `xox*`), `Bearer` tokens, and URL basic-auth credentials:

```bash
# Export portable monolithic JSON (redacted) to file or stdout
traceport export <session_id> --format json --redact --out fixture.json

# Generate a Markdown flight audit report with metadata, checkpoint file deltas, and frame logs
traceport export <session_id> --format markdown --redact --out audit_report.md
```

### Unified Checkpoint Diff

Compare any two recorded steps, or compare a step against the current live working tree:

```bash
# Diff step 1 against live working tree
traceport diff <session_id>

# Diff step 2 against step 7
traceport diff <session_id> --from 2 --to 7

# Structured JSON diff report
traceport diff <session_id> --from 2 --to 7 --json
```

### Time-Travel Rewind

Preview or execute delta workspace restoration to step `N`. Before modifying any files, TracePort captures an automatic safety savepoint of uncommitted workspace state:

```bash
# Preview creations, modifications, and deletions without touching disk
traceport rewind <session_id> --step 5 --dry-run

# Roll back workspace and session timeline to step 5
traceport rewind <session_id> --step 5

# Override cross-workspace directory guard if intentionally restoring elsewhere
traceport rewind <session_id> --step 5 --force
```

### Deterministic Zero-Token Replay

```bash
traceport replay <session_id>
traceport replay <session_id> --from 4
```

### Train Workspace Zstandard Dictionary

Scan repository source files (automatically skipping `.git`, `.traceport`, `node_modules`, `vendor`, `dist`, and binaries) to train an optimized `.zdict` dictionary:

```bash
traceport dict train
traceport dict train --json
```

### Cryptographic Verification & Garbage Collection

```bash
# Verify SHA-256 digests of all session logs, Zstd manifests, and blobs
traceport verify
traceport verify <session_id> --json

# Preview or execute garbage collection (retain newest 10 sessions, sweep orphan blobs)
traceport gc --keep 10 --dry-run
traceport gc --keep 10
```

---

## License

Released under the [MIT License](LICENSE). Authored by [aloneangel45](https://github.com/aloneangel45).
