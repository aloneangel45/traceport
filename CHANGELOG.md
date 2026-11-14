# Changelog

All notable changes to **TracePort** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.2.1] - 2026-11-14

### Fixed
- **Zero-Module Windows Installer Compatibility (`install.ps1` & `install.sh`):**
  - Replaced `Get-FileHash`, `Expand-Archive`, and external PowerShell Utility/Management cmdlet dependencies in `install.ps1` with pure `.NET` APIs (`[System.Security.Cryptography.SHA256]::Create()`, `System.IO.Compression.ZipFile::ExtractToDirectory`, `[System.Net.WebClient]`, `[System.IO.Path]`, `[System.IO.File]`), preventing `CommandNotFoundException` in stripped `PSModulePath` / `PSModuleAutoLoadingPreference=None` environments, clearing `FILE_ATTRIBUTE_READONLY` on existing binaries before replacement, and handling nested archive paths.
  - Updated `install.sh` to extract archives into an isolated temporary subdirectory, stage binaries onto the target filesystem before `mv -f` (preventing `ETXTBSY` / read-only overwrite errors on running Unix binaries), and updated offline fallback release tags in both installers to `v1.2.1`.
- **Shadow-Tree Read-Only File Rollback, Stat-Cache Isolation & Integrity Hardening (`pkg/checkpoint`):**
  - Cleared read-only file and directory bits (`0444` / `FILE_ATTRIBUTE_READONLY`) before deletion or atomic rename during `RestoreSnapshotWithStats` and `writeAtomicFile`, fixing `Access is denied` failures on Windows and read-only directory rollbacks on POSIX, and delayed target `0444` mode application until after atomic file rename so failed writes never leak read-only `.tp-tmp-*` files.
  - Pruned untracked post-checkpoint empty directories bottom-up during `RestoreSnapshotWithStats` while preserving `.git`, `.traceport`, and active store directories, and skipped transient `.tp-*` temporary files during snapshot scans.
  - Upgraded `trees/stat_cache.json` to workspace-scoped v2 format keyed by normalized workspace root (guarded by mutex), eliminating cross-workspace SHA-256 blob cache collisions when multiple workspaces share `TRACEPORT_HOME` or the OS config store.
  - Added outer `sha256(decoded) == treeHash` verification, duplicate path rejection, and negative file size validation inside `LoadManifest`, plus cross-OS rejection of Windows drive letters (`C:/...`) and colons (`:`) in `validateRelPath` on Linux, macOS, and Windows.
- **Crash-Truncated JSONL Recovery Before Append (`pkg/storage`):**
  - Repaired `.frames.jsonl` files missing a trailing newline (e.g., after a prior `SIGKILL` mid-line write or non-newline-terminated import) via atomic rewrite in `AppendFrameWithIndex` before `O_APPEND` writes, preventing subsequent frames from concatenating onto partial trailing fragments and corrupting valid frames.
  - Cleared read-only attributes before removing session artifacts in `DeleteSession` and added deterministic session ID tie-breaking in `ListSessions`.
- **Expanded Credential Redaction Coverage (`pkg/redact`):**
  - Added scrubbing for PEM/OpenSSH/PGP private key blocks (complete and crash-truncated), URL query-string secrets (`?api_key=`, `&token=`, `&access_token=`, `&client_secret=`, `&password=`, etc.), `Authorization: Basic ...`, `X-API-Key` headers, CLI/env secret assignments (`AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `CLIENT_SECRET`, `DATABASE_PASSWORD`, etc.), Stripe (`sk_live_*`, `rk_live_*`, `whsec_*`), GitLab (`glpat-*`), npm (`npm_*`), and SendGrid (`SG.*`) tokens.
  - Scrubbed `Session.WorkspaceDir`, expanded sensitive map field names (`x_api_key`, `webhook_secret`, `token`, `api_keys`, `cookie`, etc.), and masked string slices (`[]string`, `[]interface{}`) nested under sensitive tool argument keys.
- **Safe Garbage Collection, Live Diff Path Boundaries & Markdown Export Escaping (`pkg/replay`):**
  - Hardened `Engine.GC` to enumerate raw session IDs via `ListSessionIDs()` and verify every retained session and tree manifest loads cleanly before pruning, aborting safely rather than deleting referenced Zstd trees/blobs on unreadable metadata or manifests.
  - Fixed `readLiveWorkspaceFiles` in `pkg/replay/diff.go` using path-boundary containment (`isPathWithinDir`) instead of raw string prefix matching (which previously misclassified sibling files like `.traceport-ci.yaml` as deleted), skipped transient `.tp-*` files, and rejected negative `--from`/`--to` step numbers.
  - Fixed Markdown export in `pkg/replay/export.go` to truncate excerpts on valid UTF-8 rune boundaries instead of raw byte offsets, dynamically lengthen tilde code fences (`~~~~`+) when output contains tildes, and load tree manifests directly from `store.GetRootDir()` without `os.Getwd()` fallbacks across `export` and `search`.
  - Validated missing snapshots during `rewind --dry-run`, reset `session.ExitCode` to `0` upon successful rewind, and rejected out-of-bounds `--step` values in `replay`.
- **CLI Flag Reordering, Positional Validation & Recorder Signal Exit Codes (`cmd/traceport` & `pkg/recorder`):**
  - Fixed `reorderFlagsFirst` in `cmd/traceport/main.go` to preserve `--` when forwarding to `flag.FlagSet` so leading-dash queries (`traceport search -- -ldflags`) are parsed as positional arguments instead of unknown flags, supported flags before `train` in `traceport dict`, validated unexpected extra positional arguments across subcommands, and propagated `session.ExitCode` for aborted sessions.
  - Fixed Unix signal-terminated child exit codes (`128 + signal` instead of `-1`), guarded non-positive checkpoint intervals against `time.NewTicker` panics, and returned the persisted aborted `model.Session` when child process startup fails in `recorder.Runner.Record`.

## [1.2.0] - 2026-11-14

### Added
- **Session Tagging & Filtering (`record --tag`, `tag`, `list --tag`):**
  - Added normalized, validated, deduplicated `Tags []string` (`json:"tags,omitempty"`) metadata to `model.Session` and atomic `<id>.meta.json` storage.
  - Added `--tag <tag>` (repeatable or comma-separated) to `traceport record` so flight sessions can be tagged at recording time.
  - Added `traceport tag <session_id> <tag...> [--remove] [--json]` subcommand to add or remove tags on existing sessions in `O(1)` metadata updates.
  - Added `--tag <tag>` filter flag to `traceport list` / `sessions` across both human-readable tables and `--json` output.
- **Portable Session Export, Secret Redaction & Markdown Audit Reports (`traceport export` & `pkg/redact`):**
  - Added pure-Go zero-dependency credential scrubber (`pkg/redact`) covering OpenAI (`sk-*`, `sk-proj-*`), Anthropic (`sk-ant-*`), GitHub (`ghp_*`, `gho_*`, `ghs_*`, `github_pat_*`), Google (`AIza*`), AWS (`AKIA*`, `ASIA*`), HuggingFace (`hf_*`), Slack (`xox[baprs]-*`), `Bearer <token>` headers, and URL basic-auth credentials (`scheme://user:pass@host`).
  - Added `traceport export <session_id> [--format json|markdown] [--out <path>] [--redact]`:
    - `--format json` (default): Exports a self-contained monolithic `model.Session` JSON combining `.meta.json` and `.frames.jsonl` for CI artifacts and `agentfixture import -traceport <file>`.
    - `--format markdown` (alias `md`): Generates structured Markdown flight audit reports with session metadata tables, shadow-tree checkpoint & file delta summaries, and chronological frame/tool logs.
    - `--redact`: Deep-clones and scrubs sensitive credentials across commands, frame contents, and tool invocation arguments/results before writing to stdout or atomically to `--out <path>`.
- **Cross-Session Search (`traceport search`):**
  - Added `traceport search <query> [--regex] [--tag <tag>] [--json]` to scan recorded sessions across command strings, tags, frame contents, tool invocations, and shadow-tree checkpoint manifest file mutations (`added`, `modified`, `deleted`), supporting both case-insensitive substring matching and `--regex` regular expressions.

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
