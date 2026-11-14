package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/recorder"
	"github.com/aloneangel45/traceport/pkg/replay"
	"github.com/aloneangel45/traceport/pkg/storage"
)

// Version is injected at link time via -ldflags "-X main.Version=...".
var Version = "1.2.1"

func displayVersion() string {
	v := strings.TrimSpace(Version)
	if v == "" {
		return "v1.2.1"
	}
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(val string) error {
	*s = append(*s, val)
	return nil
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
	case "tag":
		return handleTag(store, subArgs, stdout, stderr)
	case "inspect":
		return handleInspect(store, subArgs, stdout, stderr)
	case "export":
		return handleExport(store, subArgs, stdout, stderr)
	case "search":
		return handleSearch(store, subArgs, stdout, stderr)
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
	fmt.Fprintln(out, "  record [--tag <tag>] -- <cmd> [args...]   Record an agent or CLI execution session")
	fmt.Fprintln(out, "  list | sessions [--tag <tag>] [--json]    List recorded flight sessions")
	fmt.Fprintln(out, "  tag <session_id> <tag...> [--remove]      Add or remove tags on an existing session")
	fmt.Fprintln(out, "  inspect <session_id> [--json]             Inspect session frames and metadata")
	fmt.Fprintln(out, "  export <session_id> [--format json|md]    Export portable session JSON or audit report")
	fmt.Fprintln(out, "  search <query> [--regex] [--tag <tag>]    Search across commands, frames, and checkpoints")
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
// parses flags regardless of whether they appear before or after <session_id>, while
// preserving '--' when explicit end-of-flags or leading-dash positional tokens are present.
func reorderFlagsFirst(args []string, valueFlags map[string]bool) []string {
	var flagsPart []string
	var posPart []string
	sawDoubleDash := false
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			sawDoubleDash = true
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
	if len(posPart) == 0 {
		return flagsPart
	}
	needsSep := sawDoubleDash
	if !needsSep {
		for _, p := range posPart {
			if strings.HasPrefix(p, "-") && p != "-" {
				needsSep = true
				break
			}
		}
	}
	if needsSep {
		out := make([]string, 0, len(flagsPart)+1+len(posPart))
		out = append(out, flagsPart...)
		out = append(out, "--")
		out = append(out, posPart...)
		return out
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
	if len(args) == 0 {
		fmt.Fprintln(stderr, "[err] missing command to record (example: traceport record [--tag <tag>] -- go test ./...)")
		return 1
	}
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport record [--tag <tag>] -- <cmd> [args...]")
		return 0
	}

	var flagTokens []string
	var cmdTokens []string

	dashIdx := -1
	for i, a := range args {
		if a == "--" {
			dashIdx = i
			break
		}
	}

	if dashIdx >= 0 {
		flagTokens = args[:dashIdx]
		cmdTokens = args[dashIdx+1:]
	} else {
		fs := flag.NewFlagSet("record", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var tagFlags stringSliceFlag
		fs.Var(&tagFlags, "tag", "Attach session tag (repeatable or comma-separated)")
		if err := fs.Parse(args); err != nil {
			return 1
		}
		return executeRecordWithTags(store, tagFlags, fs.Args(), dashIdx >= 0, stdout, stderr)
	}

	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var tagFlags stringSliceFlag
	fs.Var(&tagFlags, "tag", "Attach session tag (repeatable or comma-separated)")
	if err := fs.Parse(flagTokens); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "[err] unexpected arguments before '--': %s\n", strings.Join(fs.Args(), " "))
		return 1
	}

	return executeRecordWithTags(store, tagFlags, cmdTokens, true, stdout, stderr)
}

func executeRecordWithTags(store *storage.Store, rawTags []string, cmdTokens []string, hadDash bool, stdout, stderr io.Writer) int {
	if len(cmdTokens) == 0 {
		if hadDash {
			fmt.Fprintln(stderr, "[err] no target command specified after '--'")
		} else {
			fmt.Fprintln(stderr, "[err] missing command to record (example: traceport record -- go test ./...)")
		}
		return 1
	}

	normTags, err := storage.NormalizeTags(rawTags)
	if err != nil {
		fmt.Fprintf(stderr, "[err] %v\n", err)
		return 1
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "[err] cannot determine working directory: %v\n", err)
		return 1
	}

	runner := recorder.NewRunner(store, cwd)
	runner.Tags = normTags
	runner.Stdout = stdout
	runner.Stderr = stderr

	fmt.Fprintf(stdout, "[traceport] recording command: %s\n", strings.Join(cmdTokens, " "))
	session, cmdErr := runner.Record(cmdTokens[0], cmdTokens[1:])
	if cmdErr != nil || (session != nil && session.Status == "aborted") {
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
			if cmdErr != nil {
				fmt.Fprintf(stderr, "[err] session %s aborted (exit=%d, frames=%d): %v\n", session.ID, exitCode, len(session.Frames), cmdErr)
			} else {
				fmt.Fprintf(stderr, "[err] session %s aborted (exit=%d, frames=%d)\n", session.ID, exitCode, len(session.Frames))
			}
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
		fmt.Fprintln(stdout, "Usage: traceport list [--tag <tag>] [--json] (aliases: ls, sessions)")
		return 0
	}

	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Emit JSON output")
	var tagFlags stringSliceFlag
	fs.Var(&tagFlags, "tag", "Filter sessions by tag")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"tag": true})); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "[err] unexpected argument %q (usage: traceport list [--tag <tag>] [--json])\n", fs.Arg(0))
		return 1
	}

	filterTags, err := storage.NormalizeTags(tagFlags)
	if err != nil {
		fmt.Fprintf(stderr, "[err] %v\n", err)
		return 1
	}

	allSessions, err := store.ListSessions()
	if err != nil {
		fmt.Fprintf(stderr, "[err] failed reading sessions: %v\n", err)
		return 1
	}

	sessions := make([]model.Session, 0, len(allSessions))
	for _, s := range allSessions {
		if len(filterTags) == 0 || storage.MatchesAllTags(s, filterTags) {
			sessions = append(sessions, s)
		}
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
		if len(filterTags) > 0 {
			fmt.Fprintf(stdout, "[traceport] no sessions matched tag filter: %s\n", strings.Join(filterTags, ", "))
			return 0
		}
		fmt.Fprintln(stdout, "[traceport] no sessions recorded yet (run: traceport record -- <cmd>)")
		return 0
	}

	fmt.Fprintf(stdout, "%-26s %-16s %-7s %-20s %-18s %s\n", "SESSION ID", "STATUS", "FRAMES", "STARTED", "TAGS", "COMMAND")
	fmt.Fprintln(stdout, strings.Repeat("-", 104))

	for _, s := range sessions {
		started := s.StartTime.Format("2006-01-02 15:04:05")
		cmdDisplay := truncateRunes(s.Command, 28)
		statusDisplay := truncateRunes(s.Status, 15)
		tagsDisplay := "-"
		if len(s.Tags) > 0 {
			tagsDisplay = truncateRunes(strings.Join(s.Tags, ","), 17)
		}
		fmt.Fprintf(stdout, "%-26s %-16s %-7d %-20s %-18s %s\n", s.ID, statusDisplay, len(s.Frames), started, tagsDisplay, cmdDisplay)
	}
	return 0
}

func handleTag(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport tag <session_id> <tag...> [--remove] [--json]")
		return 0
	}

	fs := flag.NewFlagSet("tag", flag.ContinueOnError)
	fs.SetOutput(stderr)
	remove := fs.Bool("remove", false, "Remove specified tags instead of adding")
	asJSON := fs.Bool("json", false, "Emit JSON output")
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 1
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(stderr, "[err] usage: traceport tag <session_id> <tag...> [--remove] [--json]")
		return 1
	}

	sessionID := fs.Arg(0)
	rawTags := fs.Args()[1:]

	var (
		updated *model.Session
		err     error
	)
	if *remove {
		updated, err = store.RemoveSessionTags(sessionID, rawTags)
	} else {
		updated, err = store.AddSessionTags(sessionID, rawTags)
	}
	if err != nil {
		fmt.Fprintf(stderr, "[err] %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(updated); err != nil {
			fmt.Fprintf(stderr, "[err] json encode failed: %v\n", err)
			return 1
		}
		return 0
	}

	tagsLabel := "(none)"
	if len(updated.Tags) > 0 {
		tagsLabel = strings.Join(updated.Tags, ", ")
	}
	fmt.Fprintf(stdout, "[ok] session %s tags: %s\n", updated.ID, tagsLabel)
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
	if fs.NArg() != 1 {
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
	if len(session.Tags) > 0 {
		fmt.Fprintf(stdout, "Tags:       %s\n", strings.Join(session.Tags, ", "))
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

func handleExport(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport export <session_id> [--format json|markdown] [--out <path>] [--redact]")
		return 0
	}

	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "json", "Export format: json or markdown (md)")
	outPath := fs.String("out", "", "Destination file path (defaults to stdout)")
	redactFlag := fs.Bool("redact", false, "Scrub API keys, bearer tokens, and URL credentials")

	ordered := reorderFlagsFirst(args, map[string]bool{"format": true, "out": true})
	if err := fs.Parse(ordered); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "[err] usage: traceport export <session_id> [--format json|markdown] [--out <path>] [--redact]")
		return 1
	}

	sessionID := fs.Arg(0)
	eng := replay.NewEngine(store)
	payload, err := eng.ExportSession(sessionID, replay.ExportOptions{
		Format: *format,
		Redact: *redactFlag,
	})
	if err != nil {
		fmt.Fprintf(stderr, "[err] export failed: %v\n", err)
		return 1
	}

	if strings.TrimSpace(*outPath) == "" {
		if _, err := stdout.Write(payload); err != nil {
			fmt.Fprintf(stderr, "[err] write export output: %v\n", err)
			return 1
		}
		return 0
	}

	if err := writeAtomicCLIFile(*outPath, payload, 0644); err != nil {
		fmt.Fprintf(stderr, "[err] failed writing export file %q: %v\n", *outPath, err)
		return 1
	}
	fmt.Fprintf(stdout, "[ok] exported session %s (%s) -> %s\n", sessionID, strings.ToLower(strings.TrimSpace(*format)), *outPath)
	return 0
}

func handleSearch(store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprintln(stdout, "Usage: traceport search <query> [--regex] [--tag <tag>] [--json]")
		return 0
	}

	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(stderr)
	useRegex := fs.Bool("regex", false, "Interpret query as a regular expression")
	asJSON := fs.Bool("json", false, "Emit JSON search results")
	var tagFlags stringSliceFlag
	fs.Var(&tagFlags, "tag", "Restrict search to sessions matching tag")

	ordered := reorderFlagsFirst(args, map[string]bool{"tag": true})
	if err := fs.Parse(ordered); err != nil {
		return 1
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "[err] usage: traceport search <query> [--regex] [--tag <tag>] [--json]")
		return 1
	}

	query := strings.Join(fs.Args(), " ")
	eng := replay.NewEngine(store)
	results, err := eng.Search(replay.SearchOptions{
		Query: query,
		Regex: *useRegex,
		Tags:  tagFlags,
	})
	if err != nil {
		fmt.Fprintf(stderr, "[err] search failed: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			fmt.Fprintf(stderr, "[err] json encode failed: %v\n", err)
			return 1
		}
		return 0
	}

	if len(results) == 0 {
		fmt.Fprintf(stdout, "[traceport] no matches found for %q\n", query)
		return 0
	}

	totalHits := 0
	for _, res := range results {
		totalHits += len(res.Matches)
		tagSuffix := ""
		if len(res.Tags) > 0 {
			tagSuffix = fmt.Sprintf(" [tags:%s]", strings.Join(res.Tags, ","))
		}
		fmt.Fprintf(stdout, "Session %s (%s)%s -- %s\n", res.SessionID, res.Status, tagSuffix, truncateRunes(res.Command, 60))
		for _, m := range res.Matches {
			if m.Step > 0 {
				fmt.Fprintf(stdout, "  [step #%d | %s] %s\n", m.Step, m.Source, m.Snippet)
			} else {
				fmt.Fprintf(stdout, "  [%s] %s\n", m.Source, m.Snippet)
			}
		}
	}
	fmt.Fprintf(stdout, "[ok] found %d matches across %d sessions\n", totalHits, len(results))
	return 0
}

func writeAtomicCLIFile(dest string, data []byte, perm os.FileMode) error {
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	dir := filepath.Dir(absDest)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tp-export-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Chmod(tmpName, 0600)
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(perm); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, absDest); err != nil {
		_ = os.Chmod(absDest, 0600)
		if retryErr := os.Rename(tmpName, absDest); retryErr != nil {
			return retryErr
		}
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(absDest, perm)
	}
	cleanup = false
	return nil
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
	if fs.NArg() != 1 || *step <= 0 {
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
	if fs.NArg() != 1 || *fromStep <= 0 {
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
	if fs.NArg() != 1 || *fromStep <= 0 || *toStep < 0 {
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
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "[err] usage: traceport verify [<session_id>] [--json]")
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
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "[err] usage: traceport gc [--keep <N>] [--dry-run] [--json]")
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

	fs := flag.NewFlagSet("dict train", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Emit JSON dictionary stats")
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 1
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "[err] usage: traceport dict train [--json]")
		return 1
	}
	sub := strings.ToLower(fs.Arg(0))
	if sub != "train" {
		fmt.Fprintf(stderr, "[err] unknown dict subcommand %q (expected: train)\n", fs.Arg(0))
		return 1
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "[err] usage: traceport dict train [--json]")
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
