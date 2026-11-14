package recorder

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

func TestRecorderHelperProcess(t *testing.T) {
	if os.Getenv("TRACEPORT_TEST_HELPER") != "1" {
		return
	}

	var args []string
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(2)
	}

	switch args[0] {
	case "pwd_and_args":
		wd, _ := os.Getwd()
		fmt.Printf("WD=%s\n", wd)
		fmt.Printf("ARGS=%s\n", strings.Join(args[1:], "|"))
		os.Exit(0)

	case "huge_lines":
		bigOut := "OUT:" + strings.Repeat("X", 1200*1024) // ~1.2 MiB single line
		bigErr := "ERR:" + strings.Repeat("Y", 300*1024)  // ~300 KiB single line
		fmt.Fprintln(os.Stdout, bigOut)
		fmt.Fprintln(os.Stderr, bigErr)
		fmt.Fprint(os.Stdout, "TRAILING_NO_NEWLINE")
		os.Exit(0)

	case "multistep_agent":
		// Step 1: write clean auth.go
		_ = os.WriteFile("auth.go", []byte("package auth\nfunc Check() bool { return true }\n"), 0644)
		fmt.Println("step 1: created clean auth.go")
		time.Sleep(80 * time.Millisecond)

		// Step 2: add handler.go
		_ = os.WriteFile("handler.go", []byte("package auth\nfunc Handle() string { return \"ok\" }\n"), 0644)
		fmt.Println("step 2: added handler.go")
		time.Sleep(80 * time.Millisecond)

		// Step 3: agent introduces broken edit and exits with code 42
		_ = os.WriteFile("auth.go", []byte("package auth\nfunc Check() bool { SYNTAX_ERROR }\n"), 0644)
		_ = os.WriteFile("stray_debug.log", []byte("temporary crash dump"), 0644)
		fmt.Fprintln(os.Stderr, "step 3: compilation failed in auth.go")
		os.Exit(42)
	}
	os.Exit(0)
}

func TestRunner_WorkingDirAndCommandFormatting(t *testing.T) {
	t.Setenv("TRACEPORT_TEST_HELPER", "1")
	workDir := t.TempDir()
	storeDir := t.TempDir()

	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(store, workDir)
	runner.Stdin = nil
	var stdoutBuf bytes.Buffer
	runner.Stdout = &stdoutBuf
	runner.Stderr = io.Discard

	args := []string{"-test.run=TestRecorderHelperProcess", "--", "pwd_and_args", "alpha", "beta"}
	sess, err := runner.Record(os.Args[0], args)
	if err != nil {
		t.Fatalf("Record failed: %v", err)
	}

	if strings.Contains(sess.Command, "[") || strings.Contains(sess.Command, "]") {
		t.Fatalf("session.Command must not contain Go slice brackets: %q", sess.Command)
	}
	if sess.Status != "completed" || sess.ExitCode != 0 {
		t.Fatalf("expected completed exit=0, got status=%s exit=%d", sess.Status, sess.ExitCode)
	}

	evalWork, _ := filepath.EvalSymlinks(workDir)
	foundWD := false
	for _, fr := range sess.Frames {
		if strings.HasPrefix(fr.Content, "WD=") {
			gotWD := strings.TrimPrefix(fr.Content, "WD=")
			evalGot, _ := filepath.EvalSymlinks(gotWD)
			if !strings.EqualFold(filepath.Clean(evalGot), filepath.Clean(evalWork)) {
				t.Fatalf("child ran in wrong directory: got %q, want %q", gotWD, workDir)
			}
			foundWD = true
		}
	}
	if !foundWD {
		t.Fatalf("did not capture WD output frame")
	}
}

func TestRunner_HugeLinesNoDeadlock(t *testing.T) {
	t.Setenv("TRACEPORT_TEST_HELPER", "1")
	workDir := t.TempDir()
	storeDir := t.TempDir()

	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(store, workDir)
	runner.Stdin = nil
	runner.Stdout = io.Discard
	runner.Stderr = io.Discard

	sess, err := runner.Record(os.Args[0], []string{"-test.run=TestRecorderHelperProcess", "--", "huge_lines"})
	if err != nil {
		t.Fatalf("Record huge_lines failed: %v", err)
	}

	var sawBigOut, sawBigErr, sawTrailing bool
	for _, fr := range sess.Frames {
		if fr.Type == model.EventOutput && strings.HasPrefix(fr.Content, "OUT:") && len(fr.Content) > 1024*1024 {
			sawBigOut = true
		}
		if fr.Type == model.EventError && strings.HasPrefix(fr.Content, "ERR:") && len(fr.Content) > 256*1024 {
			sawBigErr = true
		}
		if fr.Content == "TRAILING_NO_NEWLINE" {
			sawTrailing = true
		}
	}
	if !sawBigOut || !sawBigErr || !sawTrailing {
		t.Fatalf("missing expected large/trailing frames: bigOut=%v bigErr=%v trailing=%v", sawBigOut, sawBigErr, sawTrailing)
	}
}

func TestRunner_MultiStepCheckpointsAndExitCode(t *testing.T) {
	t.Setenv("TRACEPORT_TEST_HELPER", "1")
	workDir := t.TempDir()
	storeDir := t.TempDir()

	// Seed an initial file in non-git workspace.
	_ = os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# Demo\n"), 0644)

	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(store, workDir)
	runner.SetCheckpointInterval(25 * time.Millisecond)
	runner.Stdin = nil
	runner.Stdout = io.Discard
	runner.Stderr = io.Discard

	sess, cmdErr := runner.Record(os.Args[0], []string{"-test.run=TestRecorderHelperProcess", "--", "multistep_agent"})
	if cmdErr == nil {
		t.Fatalf("expected non-nil error for exit code 42")
	}
	if sess.Status != "aborted" {
		t.Fatalf("expected session status 'aborted', got %q", sess.Status)
	}
	if sess.ExitCode != 42 {
		t.Fatalf("expected exit code 42, got %d", sess.ExitCode)
	}

	distinctSnapshots := make(map[string]bool)
	var checkpointCount int
	for _, fr := range sess.Frames {
		if fr.Type == model.EventCheckpoint && fr.GitSnapshot != "" {
			checkpointCount++
			distinctSnapshots[fr.GitSnapshot] = true
		}
	}

	if checkpointCount < 3 || len(distinctSnapshots) < 3 {
		t.Fatalf("expected >=3 distinct checkpoints (initial, intermediate, final), got count=%d distinct=%d", checkpointCount, len(distinctSnapshots))
	}
}

func TestRunner_ZeroCheckpointIntervalAndStartFailureSession(t *testing.T) {
	t.Setenv("TRACEPORT_TEST_HELPER", "1")
	workDir := t.TempDir()
	storeDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package main\n"), 0644)

	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Struct literal / zero or negative interval must not panic in time.NewTicker.
	runner := &Runner{
		store:              store,
		workspaceDir:       workDir,
		checkpointInterval: 0,
		Stdout:             io.Discard,
		Stderr:             io.Discard,
	}
	runner.SetCheckpointInterval(-5 * time.Millisecond)

	okSess, err := runner.Record(os.Args[0], []string{"-test.run=TestRecorderHelperProcess", "--", "pwd_and_args"})
	if err != nil {
		t.Fatalf("Record with zero/negative interval failed: %v", err)
	}
	if okSess == nil || okSess.Status != "completed" {
		t.Fatalf("unexpected session after zero-interval run: %+v", okSess)
	}

	// 2. Spawn failure must still return the persisted aborted session (ExitCode=1) alongside error.
	failedSess, startErr := runner.Record("definitely-nonexistent-traceport-binary-xyz-999", []string{"--check"})
	if startErr == nil {
		t.Fatalf("expected non-nil error when binary does not exist")
	}
	if failedSess == nil {
		t.Fatalf("expected persisted aborted Session pointer on start failure, got nil")
	}
	if failedSess.Status != "aborted" || failedSess.ExitCode != 1 {
		t.Fatalf("expected aborted session with ExitCode=1, got status=%q exit=%d", failedSess.Status, failedSess.ExitCode)
	}
	reloaded, loadErr := store.GetSession(failedSess.ID)
	if loadErr != nil {
		t.Fatalf("failed loading persisted failed-start session: %v", loadErr)
	}
	if reloaded.Status != "aborted" || reloaded.ExitCode != 1 {
		t.Fatalf("reloaded session mismatch: status=%q exit=%d", reloaded.Status, reloaded.ExitCode)
	}
}

