package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/recorder"
	"github.com/aloneangel45/traceport/pkg/replay"
	"github.com/aloneangel45/traceport/pkg/storage"
)

// Version is injected at link time via -ldflags "-X main.Version=...".
var Version = "1.1.0"

func displayVersion() string {
	v := strings.TrimSpace(Version)
	if v == "" {
		return "v1.1.0"
	}
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printBanner(stdout)
		printUsage(stdout)
		return 0
	}

	command := strings.ToLower(args[0])
	switch command {
	case "help", "-h", "--help":
		printBanner(stdout)
		printUsage(stdout)
		return 0
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "traceport %s\n", displayVersion())
		return 0
	}

	store, err := storage.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "[err] failed to initialize storage: %v\n", err)
		return 1
	}

	subArgs := args[1:]
	switch command {
	case "record":
		return handleRecord(store, subArgs, stdout, stderr)
	case "list", "ls", "sessions":
		return handleList(store, subArgs, stdout, stderr)
	case "inspect":
		return handleInspect(store, subArgs, stdout, stderr)
	case "rewind":
		return handleRewind(store, subArgs, stdout, stderr)
	case "replay":
		return handleReplay(store, subArgs, stdout, stderr)
	case "diff":
		return handleDiff(store, subArgs, stdout, stderr)
	case "verify":
		return handleVerify(store, subArgs, stdout, stderr)
	case "gc":
		return handleGC(store, subArgs, stdout, stderr)
	case "dict":
		return handleDict(store, subArgs, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "[err] unknown command: %s\n\n", args[0])
		printUsage(stderr)
		return 1
	}
}

func printBanner(out io.Writer) {
	fmt.Fprintf(out, "TRACEPORT %s -- deterministic flight recorder & shadow-tree time-travel debugger\n", displayVersion())
	fmt.Fprintln(out, strings.Repeat("-", 78))
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage: traceport <command> [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Core Commands:")
	fmt.Fprintln(out, "  record -- <cmd> [args...]                 Record an agent or CLI execution session")
	fmt.Fprintln(out, "  list | sessions [--json]                  List recorded flight sessions")
	fmt.Fprintln(out, "  inspect <session_id> [--json]             Inspect session frames and metadata")
	fmt.Fprintln(out, "  rewind <session_id> --step <N> [flags]    Roll back workspace and timeline to step N")
	fmt.Fprintln(out, "  replay <session_id> [--from <N>]          Deterministic zero-token session playback")
	fmt.Fprintln(out, "  diff <session_id> [--from A] [--to B]     Unified filesystem diff across checkpoints")
	fmt.Fprintln(out, "  verify [<session_id>] [--json]            Verify SHA-256 manifest and zstd blob integrity")
	fmt.Fprintln(out, "  gc [--keep <N>] [--dry-run] [--json]      Sweep unreferenced zstd blobs and prune sessions")
	fmt.Fprintln(out, "  dict train [--json]                       Train workspace zstd source dictionary")
	fmt.Fprintln(out, "  version                                   Print version information")
	fmt.Fprintln(out, "  help                                      Show this help text")
}

func isHelpArg(args []string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// reorderFlagsFirst moves flag tokens ahead of positional tokens so Go's flag.FlagSet
// parses flags regardless of whether they appear before or after <session_id>.
func reorderFlagsFirst(args []string, valueFlags map[string]bool) []string {
	var flagsPart []string
	var posPart []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			posPart = append(posPart, args[i+1:]...)
			break
		}
		if strings.HasPrefix(tok, "-") && tok != "-" {
			flagsPart = append(flagsPart, tok)
			name := strings.TrimLeft(tok, "-")
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flagsPart = append(flagsPart, args[i])
			}
		} else {
			posPart = append(posPart, tok)
		}
	}
	return append(flagsPart, posPart...)
}

// truncateRunes truncates s safely on UTF-8 rune boundaries without splitting multi-byte sequences.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

func handleRecord(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "--" && (args[0] == "-h" || args[0] == "--help")) {
		if len(args) > 0 {
			fmt.Fprintln(stdout, "Usage: traceport record -- <cmd> [args...]")
			return 0
		}
		fmt.Fprintln(stderr, "[err] missing command to record (example: traceport record -- go test ./...)")
		return 1
	}

	if args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "[err] no target command specified after '--'")
		return 1
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "[err] cannot determine working directory: %v\n", err)
		return 1
	}

	runner := recorder.NewRunner(store, cwd)
	runner.Stdout = stdout
	runner.Stderr = stderr

	fmt.Fprintf(stdout, "[traceport] recording command: %s\n", strings.Join(args, " "))
	session, cmdErr := runner.Record(args[0], args[1:])
	if cmdErr != nil {
		exitCode := 1
		if session != nil && session.ExitCode > 0 {
			exitCode = session.ExitCode
		} else {
			var exitErr *exec.ExitError
			if errors.As(cmdErr, &exitErr) && exitErr.ExitCode() > 0 {
				exitCode = exitErr.ExitCode()
			}
		}
		if session != nil {
			fmt.Fprintf(stderr, "[err] session %s aborted (exit=%d, frames=%d): %v\n", session.ID, exitCode, len(session.Frames), cmdErr)
		} else {
			fmt.Fprintf(stderr, "[err] command execution failed: %v\n", cmdErr)
		}
		return exitCode
	}

	fmt.Fprintf(stdout, "[ok] session recorded: %s (%d frames)\n", session.ID, len(session.Frames))
	return 0
}

func handleList(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport list [--json] (aliases: ls, sessions)")
		return 0
	}

	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Emit JSON output")
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 1
	}

	sessions, err := store.ListSessions()
	if err != nil {
		fmt.Fprintf(stderr, "[err] failed reading sessions: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sessions); err != nil {
			fmt.Fprintf(stderr, "[err] json encode failed: %v\n", err)
			return 1
		}
		return 0
	}

	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "[traceport] no sessions recorded yet (run: traceport record -- <cmd>)")
		return 0
	}

	fmt.Fprintf(stdout, "%-26s %-18s %-8s %-20s %s\n", "SESSION ID", "STATUS", "FRAMES", "STARTED", "COMMAND")
	fmt.Fprintln(stdout, strings.Repeat("-", 88))

	for _, s := range sessions {
		started := s.StartTime.Format("2006-01-02 15:04:05")
		cmdDisplay := truncateRunes(s.Command, 30)
		statusDisplay := truncateRunes(s.Status, 17)
		fmt.Fprintf(stdout, "%-26s %-18s %-8d %-20s %s\n", s.ID, statusDisplay, len(s.Frames), started, cmdDisplay)
	}
	return 0
}

func handleInspect(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport inspect <session_id> [--json]")
		return 0
	}

	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Emit JSON output")
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 1
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "[err] usage: traceport inspect <session_id> [--json]")
		return 1
	}

	sessionID := fs.Arg(0)
	session, err := store.GetSession(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "[err] %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(session); err != nil {
			fmt.Fprintf(stderr, "[err] json encode failed: %v\n", err)
			return 1
		}
		return 0
	}

	duration := "active"
	if !session.EndTime.IsZero() {
		duration = session.EndTime.Sub(session.StartTime).Round(time.Millisecond).String()
	}

	fmt.Fprintf(stdout, "Session:    %s\n", session.ID)
	fmt.Fprintf(stdout, "Command:    %s\n", session.Command)
	if session.WorkspaceDir != "" {
		fmt.Fprintf(stdout, "Workspace:  %s\n", session.WorkspaceDir)
	}
	fmt.Fprintf(stdout, "Status:     %s (exit=%d)\n", session.Status, session.ExitCode)
	fmt.Fprintf(stdout, "Duration:   %s\n", duration)
	if session.SafetySnapshot != "" {
		fmt.Fprintf(stdout, "Savepoint:  %s\n", truncateRunes(session.SafetySnapshot, 19))
	}
	fmt.Fprintf(stdout, "Frames:     %d\n\n", len(session.Frames))

	fmt.Fprintf(stdout, "%-6s %-12s %s\n", "STEP", "TYPE", "CONTENT PREVIEW")
	fmt.Fprintln(stdout, strings.Repeat("-", 72))

	for _, f := range session.Frames {
		preview := strings.ReplaceAll(f.Content, "\n", " ")
		preview = strings.ReplaceAll(preview, "\r", "")
		preview = truncateRunes(preview, 50)
		if f.GitSnapshot != "" && len(f.GitSnapshot) >= 12 {
			preview = fmt.Sprintf("%s [tree:%s]", preview, f.GitSnapshot[:12])
		}
		fmt.Fprintf(stdout, "#%-5d %-12s %s\n", f.Index, f.Type, preview)
	}
	return 0
}

func handleRewind(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport rewind <session_id> --step <N> [--force] [--dry-run]")
		return 0
	}

	fs := flag.NewFlagSet("rewind", flag.ContinueOnError)
	fs.SetOutput(stderr)
	step := fs.Int("step", 0, "Target step to rewind to")
	force := fs.Bool("force", false, "Allow rewinding into a different workspace directory")
	dryRun := fs.Bool("dry-run", false, "Preview file mutations without modifying workspace")

	ordered := reorderFlagsFirst(args, map[string]bool{"step": true})
	if err := fs.Parse(ordered); err != nil {
		return 1
	}
	if fs.NArg() < 1 || *step <= 0 {
		fmt.Fprintln(stderr, "[err] usage: traceport rewind <session_id> --step <N> [--force] [--dry-run]")
		return 1
	}

	sessionID := fs.Arg(0)
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "[err] cannot determine working directory: %v\n", err)
		return 1
	}

	eng := replay.NewEngine(store)
	res, err := eng.RewindWithOptions(sessionID, *step, cwd, replay.RewindOptions{
		Force:  *force,
		DryRun: *dryRun,
	})
	if err != nil {
		fmt.Fprintf(stderr, "[err] rewind failed: %v\n", err)
		return 1
	}

	if *dryRun {
		fmt.Fprintf(stdout, "[traceport] dry-run rewind preview for %s -> step %d\n", sessionID, *step)
		if res.Plan != nil {
			fmt.Fprintf(stdout, "  create:    %d files\n", len(res.Plan.Create))
			for _, p := range res.Plan.Create {
				fmt.Fprintf(stdout, "    + %s\n", p)
			}
			fmt.Fprintf(stdout, "  modify:    %d files\n", len(res.Plan.Modify))
			for _, p := range res.Plan.Modify {
				fmt.Fprintf(stdout, "    ~ %s\n", p)
			}
			fmt.Fprintf(stdout, "  delete:    %d files\n", len(res.Plan.Delete))
			for _, p := range res.Plan.Delete {
				fmt.Fprintf(stdout, "    - %s\n", p)
			}
			fmt.Fprintf(stdout, "  unchanged: %d files\n", len(res.Plan.Unchanged))
		}
		return 0
	}

	if res.SafetySnapshot != "" && len(res.SafetySnapshot) >= 16 {
		fmt.Fprintf(stdout, "[ok] pre-rewind safety savepoint created: %s\n", res.SafetySnapshot[:16])
	}
	if res.RestoreStats != nil {
		fmt.Fprintf(
			stdout,
			"[ok] rewound session %s to step %d (restored=%d, deleted=%d, skipped=%d)\n",
			res.Session.ID,
			*step,
			res.RestoreStats.FilesRestored,
			res.RestoreStats.FilesDeleted,
			res.RestoreStats.FilesSkipped,
		)
	} else {
		fmt.Fprintf(stdout, "[ok] rewound session %s to step %d\n", res.Session.ID, *step)
	}
	return 0
}

func handleReplay(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport replay <session_id> [--from <N>]")
		return 0
	}

	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fromStep := fs.Int("from", 1, "Step index to start playback from")
	ordered := reorderFlagsFirst(args, map[string]bool{"from": true})
	if err := fs.Parse(ordered); err != nil {
		return 1
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "[err] usage: traceport replay <session_id> [--from <N>]")
		return 1
	}

	eng := replay.NewEngine(store)
	if err := eng.Playback(fs.Arg(0), *fromStep, stdout); err != nil {
		fmt.Fprintf(stderr, "[err] replay failed: %v\n", err)
		return 1
	}
	return 0
}

func handleDiff(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport diff <session_id> [--from <stepA>] [--to <stepB>] [--json]")
		return 0
	}

	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fromStep := fs.Int("from", 1, "Source step index (default 1)")
	toStep := fs.Int("to", 0, "Target step index (0 compares against current working tree)")
	asJSON := fs.Bool("json", false, "Emit JSON diff report")

	ordered := reorderFlagsFirst(args, map[string]bool{"from": true, "to": true})
	if err := fs.Parse(ordered); err != nil {
		return 1
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "[err] usage: traceport diff <session_id> [--from <stepA>] [--to <stepB>] [--json]")
		return 1
	}

	cwd, _ := os.Getwd()
	eng := replay.NewEngine(store)
	report, err := eng.DiffSession(fs.Arg(0), *fromStep, *toStep, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "[err] diff failed: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return 0
	}

	if len(report.Changes) == 0 {
		fmt.Fprintln(stdout, "[traceport] no filesystem changes between target states")
		return 0
	}

	fmt.Fprint(stdout, report.UnifiedText)
	return 0
}

func handleVerify(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport verify [<session_id>] [--json]")
		return 0
	}

	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Emit JSON verification report")
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 1
	}

	sessionID := ""
	if fs.NArg() > 0 {
		sessionID = fs.Arg(0)
	}

	eng := replay.NewEngine(store)
	report, err := eng.Verify(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "[err] verify failed: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		if !report.Valid {
			return 1
		}
		return 0
	}

	if !report.Valid {
		for _, e := range report.Errors {
			fmt.Fprintf(stderr, "[err] %s\n", e)
		}
		fmt.Fprintf(stderr, "[err] integrity check failed (%d errors)\n", len(report.Errors))
		return 1
	}

	fmt.Fprintf(
		stdout,
		"[ok] verified %d sessions, %d manifests, %d file references (%d bytes intact)\n",
		report.SessionsChecked,
		report.TreesVerified,
		report.FilesVerified,
		report.BytesVerified,
	)
	return 0
}

func handleGC(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport gc [--keep <N>] [--dry-run] [--json]")
		return 0
	}

	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keep := fs.Int("keep", -1, "Retain newest N sessions (-1 retains all sessions)")
	dryRun := fs.Bool("dry-run", false, "Preview reclaimed space without deleting files")
	asJSON := fs.Bool("json", false, "Emit JSON GC report")

	ordered := reorderFlagsFirst(args, map[string]bool{"keep": true})
	if err := fs.Parse(ordered); err != nil {
		return 1
	}

	eng := replay.NewEngine(store)
	report, err := eng.GC(replay.GCOptions{
		Keep:   *keep,
		DryRun: *dryRun,
	})
	if err != nil {
		fmt.Fprintf(stderr, "[err] gc failed: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return 0
	}

	modePrefix := "[ok] gc completed"
	if report.DryRun {
		modePrefix = "[traceport] gc dry-run"
	}
	fmt.Fprintf(
		stdout,
		"%s: sessions_pruned=%d trees_removed=%d blobs_removed=%d reclaimed=%dB retained_blobs=%d ratio=%.1f%%\n",
		modePrefix,
		report.SessionsPruned,
		report.TreesRemoved,
		report.BlobsRemoved,
		report.BytesReclaimed,
		report.BlobsRetained,
		report.CompressionRatioPct,
	)
	return 0
}

func handleDict(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args) {
		if len(args) > 0 && isHelpArg(args) {
			fmt.Fprintln(stdout, "Usage: traceport dict train [--json]")
			return 0
		}
		fmt.Fprintln(stderr, "[err] usage: traceport dict train [--json]")
		return 1
	}

	sub := strings.ToLower(args[0])
	if sub != "train" {
		fmt.Fprintf(stderr, "[err] unknown dict subcommand %q (expected: train)\n", args[0])
		return 1
	}

	fs := flag.NewFlagSet("dict train", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Emit JSON dictionary stats")
	if err := fs.Parse(reorderFlagsFirst(args[1:], nil)); err != nil {
		return 1
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "[err] cannot determine working directory: %v\n", err)
		return 1
	}

	stats, err := checkpoint.TrainDictionaryFromDir(cwd, store.RootDir())
	if err != nil {
		fmt.Fprintf(stderr, "[err] dictionary training failed: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(stats)
		return 0
	}

	fmt.Fprintf(
		stdout,
		"[ok] trained zstd dictionary id=0x%08x size=%dB samples=%d raw_ratio=%.1f%% dict_ratio=%.1f%% time=%s\n",
		stats.DictID,
		stats.DictBytes,
		stats.SampleCount,
		stats.RatioRawPct,
		stats.RatioDictPct,
		stats.Duration.Round(time.Millisecond),
	)
	fmt.Fprintf(stdout, "[traceport] persisted dictionary: %s\n", stats.DictPath)
	return 0
}
