package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aloneangel45/traceport/pkg/recorder"
	"github.com/aloneangel45/traceport/pkg/replay"
	"github.com/aloneangel45/traceport/pkg/storage"
)

const Version = "1.0.0"

func main() {
	if len(os.Args) < 2 {
		printBanner()
		printUsage()
		return
	}

	store, err := storage.NewStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing storage: %v\n", err)
		os.Exit(1)
	}

	command := strings.ToLower(os.Args[1])

	switch command {
	case "record":
		handleRecord(store, os.Args[2:])
	case "list", "ls":
		handleList(store)
	case "inspect":
		handleInspect(store, os.Args[2:])
	case "rewind":
		handleRewind(store, os.Args[2:])
	case "replay":
		handleReplay(store, os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("TracePort v%s (AI Agent Flight Recorder & Time-Travel Debugger)\n", Version)
	default:
		printBanner()
		fmt.Printf("Unknown command: %s\n\n", command)
		printUsage()
	}
}

func printBanner() {
	banner := `
  ████████╗██████╗  █████╗  ██████╗███████╗██████╗  ██████╗ ██████╗ ████████╗
  ╚══██╔══╝██╔══██╗██╔══██╗██╔════╝██╔════╝██╔══██╗██╔═══██╗██╔══██╗╚══██╔══╝
     ██║   ██████╔╝███████║██║     █████╗  ██████╔╝██║   ██║██████╔╝   ██║   
     ██║   ██╔══██╗██╔══██║██║     ██╔══╝  ██╔═══╝ ██║   ██║██╔══██╗   ██║   
     ██║   ██║  ██║██║  ██║╚██████╗███████╗██║     ╚██████╔╝██║  ██║   ██║   
     ╚═╝   ╚═╝  ╚═╝╚═╝  ╚═╝ ╚═════╝╚══════╝╚═╝      ╚═════╝ ╚═╝  ╚═╝   ╚═╝   
`
	fmt.Println(banner)
	fmt.Println("  🛰️  AI Agent Flight Recorder & Time-Travel Replay Engine  v1.0.0")
	fmt.Println("  " + strings.Repeat("─", 72) + "\n")
}

func printUsage() {
	fmt.Println("Usage: traceport <command> [options]")
	fmt.Println("\nAvailable Commands:")
	fmt.Println("  record -- <cmd> [args...]   Wrap and record an agent execution session")
	fmt.Println("  list                        List all recorded flight sessions")
	fmt.Println("  inspect <session_id>        Inspect telemetry and step-by-step frames of a session")
	fmt.Println("  rewind <session_id> --step  Roll back workspace and timeline to step N")
	fmt.Println("  replay <session_id>         Deterministic zero-token playback of a recorded session")
	fmt.Println("  version                     Print version information")
}

func handleRecord(store *storage.Store, args []string) {
	if len(args) == 0 {
		fmt.Println("Error: must provide a command to record. Example: traceport record -- go test ./...")
		return
	}

	// Filter out potential leading "--"
	if args[0] == "--" {
		args = args[1:]
	}

	if len(args) == 0 {
		fmt.Println("Error: no target command specified after '--'")
		return
	}

	cwd, _ := os.Getwd()
	runner := recorder.NewRunner(store, cwd)

	fmt.Printf("  • Starting TracePort Flight Recorder for [%s]...\n", strings.Join(args, " "))
	session, err := runner.Record(args[0], args[1:])
	if err != nil {
		fmt.Printf("\n  ✘ Command execution ended with status: %v\n", err)
	} else {
		fmt.Printf("\n  ✔ Flight successfully recorded. Session ID: %s (%d frames)\n", session.ID, len(session.Frames))
	}
}

func handleList(store *storage.Store) {
	sessions, err := store.ListSessions()
	if err != nil {
		fmt.Printf("Error reading sessions: %v\n", err)
		return
	}

	if len(sessions) == 0 {
		fmt.Println("No flight sessions recorded yet. Run: traceport record -- <cmd>")
		return
	}

	fmt.Printf("%-24s %-12s %-8s %-20s %s\n", "SESSION ID", "STATUS", "FRAMES", "STARTED", "COMMAND")
	fmt.Println(strings.Repeat("─", 80))

	for _, s := range sessions {
		durationStr := s.StartTime.Format("2006-01-02 15:04:05")
		cmdDisplay := s.Command
		if len(cmdDisplay) > 28 {
			cmdDisplay = cmdDisplay[:25] + "..."
		}
		fmt.Printf("%-24s %-12s %-8d %-20s %s\n", s.ID, s.Status, len(s.Frames), durationStr, cmdDisplay)
	}
}

func handleInspect(store *storage.Store, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: traceport inspect <session_id>")
		return
	}

	sessionID := args[0]
	session, err := store.GetSession(sessionID)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	duration := "active"
	if !session.EndTime.IsZero() {
		duration = session.EndTime.Sub(session.StartTime).Round(time.Millisecond).String()
	}

	fmt.Printf("\nFlight Session Report: %s\n", session.ID)
	fmt.Printf("  Command:  %s\n", session.Command)
	fmt.Printf("  Status:   %s\n", session.Status)
	fmt.Printf("  Duration: %s\n", duration)
	fmt.Printf("  Frames:   %d\n\n", len(session.Frames))

	fmt.Printf("%-6s %-12s %s\n", "STEP", "TYPE", "CONTENT PREVIEW")
	fmt.Println(strings.Repeat("─", 65))

	for _, f := range session.Frames {
		preview := strings.ReplaceAll(f.Content, "\n", " ")
		if len(preview) > 45 {
			preview = preview[:42] + "..."
		}
		fmt.Printf("#%-5d %-12s %s\n", f.Index, f.Type, preview)
	}
}

func handleRewind(store *storage.Store, args []string) {
	if len(args) < 2 {
		fmt.Println("Usage: traceport rewind <session_id> --step <N>")
		return
	}

	sessionID := args[0]
	fs := flag.NewFlagSet("rewind", flag.ExitOnError)
	step := fs.Int("step", 0, "Target step to rewind to")
	_ = fs.Parse(args[1:])

	if *step <= 0 {
		fmt.Println("Error: please specify a valid target step using --step <N>")
		return
	}

	cwd, _ := os.Getwd()
	eng := replay.NewEngine(store)

	session, err := eng.Rewind(sessionID, *step, cwd)
	if err != nil {
		fmt.Printf("Rewind failed: %v\n", err)
		return
	}

	fmt.Printf("✔ Successfully rewound session %s to step %d. State restored.\n", session.ID, *step)
}

func handleReplay(store *storage.Store, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: traceport replay <session_id> [--from <N>]")
		return
	}

	sessionID := args[0]
	fromStep := 1
	if len(args) > 2 && args[1] == "--from" {
		if val, err := strconv.Atoi(args[2]); err == nil && val > 0 {
			fromStep = val
		}
	}

	eng := replay.NewEngine(store)
	if err := eng.Playback(sessionID, fromStep, os.Stdout); err != nil {
		fmt.Printf("Replay failed: %v\n", err)
	}
}
