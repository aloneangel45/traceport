# Changelog

All notable changes to **TracePort** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.1.0] - 2026-11-10

### Added
- **Zstandard Content-Addressed Shadow Trees (RFC #1):** Pure-Go (`CGO_ENABLED=0`) high-frequency workspace snapshot engine (`pkg/checkpoint`) powered by `github.com/klauspost/compress/zstd` (`SpeedFastest`) that operates in any directory (Git or non-Git) without mutating `.git/index`.
- **Deterministic Multi-Language & Workspace Zstd Dictionaries:** Built-in zero-cold-start dictionary (`DefaultSourceDict`) trained over Go, Python, TypeScript/JS, Rust, C, JSON, YAML, Markdown, Shell, and SQL constructs, plus `traceport dict train` for custom per-repository `.zdict` training.
- **Incremental Stat Cache & Fast Delta Rollback:** Stat cache (`mtime`, `size`, `mode` -> `sha256`) eliminates disk reads and re-compression for unchanged files during high-frequency recording; delta `RestoreSnapshot` skips unchanged files and removes post-checkpoint untracked artifacts while preserving `.git` and `.traceport`.
- **Continuous Multi-Step Checkpointing:** `recorder.Runner` captures initial, debounced intermediate, and final post-execution workspace checkpoints so `rewind --step <N>` restores exact intermediate agent states.
- **Pre-Rewind Safety Savepoints & Dry-Run Preview:** `traceport rewind` automatically captures a pre-rewind safety snapshot before modifying workspace files, blocks accidental cross-repository rewinds unless `--force` is passed, and supports `--dry-run`.
- **New Inspection & Maintenance Commands:**
  - `traceport diff <session_id> [--from A] [--to B] [--json]`: Pure-Go unified filesystem and line-level diff across any two checkpoints or the live working tree.
  - `traceport verify [<session_id>] [--json]`: Cryptographic SHA-256 verification of session logs, compressed manifests, and content-addressed blobs.
  - `traceport gc [--keep <N>] [--dry-run] [--json]`: Garbage collection for old sessions and unreferenced Zstd blobs/manifests with compression ratio reporting.
  - `traceport dict train [--json]`: Workspace source dictionary trainer.
- **CLI Enhancements:** Added `sessions` alias, `--help`/`-h` support across all subcommands, `--json` structured output on `list`/`inspect`/`diff`/`verify`/`gc`/`dict`, and `windows/arm64` release artifacts.

### Changed
- **Append-Only JSONL Session Store:** Replaced monolithic `O(N^2)` full-file JSON rewrites with O(1) append-only `<id>.frames.jsonl` logs and atomic `<id>.meta.json` metadata (`0700` dirs, `0600` files) with transparent backward compatibility for v1.0 `<id>.json` sessions.
- **Unified Store Discovery:** `TRACEPORT_HOME` override, nearest `.traceport`/`.git` workspace root discovery, and cross-platform OS user config fallback.

### Fixed
- Eliminated 64 KiB `bufio.Scanner` pipe deadlock on large agent JSON/tool lines by streaming stdout/stderr and JSONL logs with `bufio.Reader`.
- Fixed child process working directory propagation (`cmd.Dir = workspaceDir`), command string formatting (removed Go slice brackets), `SIGINT`/`SIGTERM` forwarding, and child exit code propagation.
- Hardened session ID validation against path traversal, isolated legacy Git index operations via temporary `GIT_INDEX_FILE` paths, blocked symlink directory escapes on restore, and fixed UTF-8 rune boundary string truncation.
- Fixed `install.ps1` ARM64 archive naming, added mandatory SHA-256 `checksums.txt` verification to `install.sh` and `install.ps1`, fixed `-ldflags "-X main.Version=..."` link-time version injection, and guarded `release.yml` publishing on `refs/tags/v*`.

## [1.0.0] - 2026-11-05

### Added
- Deterministic flight recorder engine for autonomous AI coding agents.
- Shadow-tree checkpointing engine with filesystem state tracking.
- Reversible execution replay and state rollback primitives.
- Automated CI pipeline with multi-platform cross-compilation binaries.
- Agent action auditing and session inspection CLI.
