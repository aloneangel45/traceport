# Changelog

All notable changes to **TracePort** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.3.1] - 2026-11-21

### Fixed
- **MCP Server JSON-RPC 2.0, Multi-Header Framing, Redaction & Size-Cap Hardening (`pkg/mcpserver`, `pkg/replay`, `pkg/redact`):**
  - Fixed `"id": null` JSON-RPC 2.0 requests being silently dropped as notifications due to Go `encoding/json` decoding `null` into `nil` `json.RawMessage`; requests are now parsed via a raw field map to distinguish omitted `"id"` from `"id": null`.
  - Enforced strict JSON-RPC 2.0 validation: non-object payloads (`123`, `null`, etc.), missing/wrong `"jsonrpc"`, invalid `"id"` types, and oversized batches (`> 100`) now return `-32600 Invalid Request`; trailing garbage returns `-32700 Parse error`; malformed `params` return `-32602 Invalid params`.
  - Fixed `Content-Length` stdio framing when other MIME headers (e.g. `Content-Type`) precede `Content-Length`, added bounded header line reads (`16 KiB` / max `64` headers), and rejected conflicting duplicate `Content-Length` headers and oversized frames.
  - Closed a `search_traces` secret-extraction oracle by scrubbing sessions with `pkg/redact` before query matching (`SearchOptions.Redact`), extended `redact.Session` to scrub `Tags`, `Status`, and `ID`, and redacted all MCP error messages and `rewind_workspace` restore plans.
  - Enforced response truncation caps across `inspect_session`, `diff_checkpoints`, `search_traces`, and `rewind_workspace` (redacting before truncating) so massive stdout/diffs never blow up LLM context windows.
  - Made pre-rewind safety savepoints strictly fail-closed in `Engine.RewindWithOptions` and guaranteed creation for every live (`dry_run=false`) rewind, including sessions without prior checkpoint snapshots.
- **Interactive Dark HTML Timeline Export Hardening (`pkg/replay/html.go`):**
  - Added strict per-frame, per-tool, pre-diff blob (`512 KiB`), per-file diff, file-count (`200`), and total-report size budgets so huge sessions (e.g. multi-MB stdout or generated files) produce fast, bounded, usable HTML reports with explicit truncation notices.
  - Added `<meta http-equiv="Content-Security-Policy" ...>`, sanitized CSS class tokens for `Frame.Type`, bound panels by unique `data-panel-idx` (preventing duplicate `Frame.Index` collisions), enabled sidebar search across changed file paths in inspector panels, and hid panels cleanly on empty search matches.
- **Workspace Watcher & `.gitignore` / `.traceportignore` Hardening (`pkg/recorder/watch.go`, `pkg/checkpoint/ignore.go`):**
  - Fixed cross-platform Windows backslash matching in `IgnoreMatcher.ShouldIgnore` / `CanSkipDir` on Linux/macOS, stripped UTF-8 BOM on line 1, preserved escaped trailing spaces (`\ `), normalized Windows backslash ignore rules, fixed subdirectory matching under ignored directories, fixed `CanSkipDir` pruning when glob negations (`!*/keep.txt`, `!a/*/keep.txt`) are present, and fixed trailing `/**` to match descendants only.
  - Fixed `recorder.Watch` advancing `lastCommittedFP` before `recordCheckpoint` succeeded (which previously dropped checkpoints after transient file locks), added deterministic `ctx.Err()` shutdown checks, and flushed unpolled final workspace mutations on `ctx.Done()`.
- **IDE Preset JSONC & Serialization Hardening (`pkg/preset/preset.go`):**
  - Stripped UTF-8 BOM from existing IDE configs, rejected unterminated `/*` block comments and trailing JSON garbage, preserved 64-bit integer precision via `json.Decoder.UseNumber()`, and disabled Go HTML escaping (`SetEscapeHTML(false)`) so existing server URLs/args containing `&`, `<`, or `>` are never corrupted into `\u0026`/`\u003c`.

## [1.3.0] - 2026-11-21

### Added
- **Native Self-Healing MCP Server (`pkg/mcpserver` & `traceport mcp`):**
  - Added a pure-Go JSON-RPC 2.0 stdio Model Context Protocol server (`initialize`, `notifications/initialized`, `ping`, `tools/list`, `tools/call`, `resources/list`, `prompts/list`) with automatic NDJSON and `Content-Length` wire-framing detection for Claude Code, Cursor, VS Code, Windsurf, and Antigravity.
  - Exposed 5 core time-travel tools: `list_sessions`, `inspect_session`, `diff_checkpoints`, `search_traces`, and `rewind_workspace` (always creating the mandatory pre-rewind safety savepoint unless `dry_run=true`).
  - Automatically passes all returned frame contents, command strings, workspace paths, search snippets, and unified checkpoint diffs through `pkg/redact` so `.env` secrets and API keys never leak into LLM contexts.
- **One-Command IDE Preset Installer (`pkg/preset` & `traceport preset`):**
  - Added `traceport preset <claude|cursor|vscode|windsurf> [--write] [--path <custom>] [--binary <path>] [--dry-run]` to preview or atomically merge the `"traceport"` MCP server entry into existing IDE JSON/JSONC configs while preserving other configured servers and settings.
- **Self-Contained Interactive Dark-Mode HTML Timeline Export (`pkg/replay/html.go` & `traceport export --format html`):**
  - Extended `traceport export <session_id> --format html [--out report.html]` to emit a zero-CDN, Catppuccin/Obsidian dark-mode interactive HTML timeline with session metadata headers, clickable step filters (`all`, `checkpoint`, `output`, `error`, `tool_call`), keyboard step navigation, redacted stdout/stderr and tool panels, and syntax-colored consecutive checkpoint unified diffs (`+` green, `-` red, `@@` cyan).
  - Enforces automatic `pkg/redact` credential scrubbing and `html.EscapeString` escaping across all untrusted content and pre-rendered step panels (zero `innerHTML`, zero XSS).
- **Continuous Workspace Directory Watcher (`pkg/recorder/watch.go`, `pkg/checkpoint/ignore.go`, & `traceport watch`):**
  - Added `traceport watch [--interval 2s] [--debounce 500ms] [--max-snapshots 200] [--tag <tag>] [--dir <path>]` to continuously snapshot workspace mutations alongside AI coding IDEs.
  - Added `.gitignore` and `.traceportignore` rule matching plus default cache/dependency filtering (`node_modules`, `.git`, `target`, `.traceport`, `.next`, `__pycache__`, `.venv`, `venv`) via `checkpoint.WithIgnoreFiltering()`, quiet-window burst coalescing (`--debounce`), tree-hash deduplication, `--max-snapshots` capping, and graceful `SIGINT`/`SIGTERM` completion.

### Security
- Expanded `pkg/redact` environment-variable secret detection to cover generic `.env` credential assignments (`*_API_KEY`, `*_SECRET_KEY`, `*_ACCESS_KEY`, `*_SECRET`, `*_PASSWORD`, `*_TOKEN`, `*_PRIVATE_KEY`).

## [1.2.2] - 2026-11-15

### Fixed
- **Cross-Platform Line Endings, Installer Hardening & Node 24 CI Workflows (`.gitattributes`, `install.sh`, `install.ps1`, `.github/workflows`):**
  - Added `.gitattributes` enforcing LF (`\n`) on `*.sh`, `*.go`, `*.yml`, and `*.yaml` files and CRLF (`\r\n`) on `*.ps1` scripts, with automated regression checks verifying `install.sh` contains zero `\r` bytes.
  - Hardened `install.sh` to run `mkdir -p "${INSTALL_DIR}"` across both root (`/usr/local/bin`) and non-root branches, stage extracted binaries into temporary files inside `${INSTALL_DIR}` before atomic `mv -f`, and clean up staged artifacts on exit.
  - Hardened `install.ps1` inside an `Install-TracePort` function scope using `throw` instead of script-scope `exit` (so `irm | iex` never terminates the caller's shell), unconditionally prepending `$PSHome\Modules` and stripping `PowerShell\7` module paths when running under Windows PowerShell `5.1` (`Desktop`), bitwise-ORing `Tls12`, and clearing `ReadOnly` attributes before binary replacement.
  - Added `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24: "true"` to `.github/workflows/ci.yml` and `.github/workflows/release.yml`, and added explicit `permissions: contents: read` to `.github/workflows/ci.yml`.
- **Symlinked Workspaces, Safe Symlink Rewind & Racy-Clean Stat-Cache Protection (`pkg/checkpoint`, `pkg/replay`):**
  - Fixed symlinked and junction workspace root traversal by resolving and walking canonical `realWorkspace` paths (`filepath.EvalSymlinks`) across `TreeStore`, `collectWorkspaceSamples` (`dict train`), and `readLiveWorkspaceFiles` (`diff`), preventing 0-file snapshots in symlinked workspaces.
  - Fixed `RestoreSnapshotWithStats` and `PlanRestore` so pre-existing non-colliding untracked workspace symlinks are preserved, while colliding post-checkpoint symlinks (including directory symlinks replacing tracked directories such as `src -> /tmp/x`) and symlinks inside newly created directories are unlinked safely before restoring files underneath them without traversing outside the workspace.
  - Fixed `statCache` racy-clean staleness by bypassing `statCache` hits within a 2-second racy-clean window (`|now - mtime| < 2s`) and verifying `f.Stat()` post-read consistency (re-reading if `preStat != postStat`), ensuring rapid `<40ms` same-size overwrites and immediate post-restore edits always record accurate SHA-256 digests.
- **Zstd Codec Close Guards & Uncompressed `.zst` Blob Integrity (`pkg/checkpoint`):**
  - Guarded `c.enc == nil` and `c.dec == nil` under `c.mu.RLock()` across `Compressor.Compress`, `Decompress`, and `DecompressAuto` so calls after `Close()` return `ErrCompressorClosed` cleanly instead of panicking.
  - Fixed `ModeUncompressed` handling of real `.zst` user workspace files across `ReadBlob`, `readBlobLocked`, `LoadManifest`, and `VerifyTree` by returning raw bytes directly whenever `sha256(raw) == expectedSHA` without invoking `DecompressAuto`.
- **Grandchild Pipe Hang Timeout, Windows Atomic Rename Retries & Active Session GC Safety (`pkg/recorder`, `pkg/storage`, `pkg/replay`):**
  - Configured `cmd.WaitDelay = 3 * time.Second` with pipe-backed `cmd.Stdout`/`cmd.Stderr` draining in `Runner.Record` so background grandchild processes holding inherited stdio descriptors cannot hang session recording indefinitely after child exit.
  - Added 5-attempt `25ms` backoff retries around `os.Rename` in `writeAtomicFile` (`pkg/checkpoint/tree.go` and `pkg/storage/store.go`) to tolerate transient Windows antivirus/indexer file-sharing locks.
  - Hardened `Engine.GC` to never prune active sessions with `Status == "running"` and to skip deleting unreferenced CAS blobs/manifests while any session is `"running"` or when modified within the last 5 minutes.

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
