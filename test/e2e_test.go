package test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aloneangel45/traceport/pkg/recorder"
	"github.com/aloneangel45/traceport/pkg/replay"
	"github.com/aloneangel45/traceport/pkg/storage"
)

func TestTracePort_LiveRecordAndReplay(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := storage.NewStoreWithPath(tmpDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	runner := recorder.NewRunner(store, tmpDir)

	cmdName := "cmd.exe"
	cmdArgs := []string{"/c", "echo", "HELLO_TRACEPORT_TEST"}
	if runtime.GOOS != "windows" {
		cmdName = "sh"
		cmdArgs = []string{"-c", "echo HELLO_TRACEPORT_TEST"}
	}

	session, err := runner.Record(cmdName, cmdArgs)
	if err != nil {
		t.Fatalf("record failed: %v", err)
	}

	if session.Status != "completed" {
		t.Errorf("expected session completed, got %s", session.Status)
	}
	if len(session.Frames) == 0 {
		t.Errorf("expected at least 1 recorded frame")
	}

	eng := replay.NewEngine(store)
	var out bytes.Buffer
	if err := eng.Playback(session.ID, 1, &out); err != nil {
		t.Fatalf("playback failed: %v", err)
	}

	if !bytes.Contains(out.Bytes(), []byte("HELLO_TRACEPORT_TEST")) {
		t.Errorf("expected replay to contain command output, got: %s", out.String())
	}
}

func TestTracePort_CompiledBinaryFullLifecycle(t *testing.T) {
	binExt := ""
	if runtime.GOOS == "windows" {
		binExt = ".exe"
	}
	binPath := filepath.Join(t.TempDir(), "traceport"+binExt)

	buildCmd := exec.Command("go", "build", "-ldflags=-X main.Version=1.1.0-e2e", "-o", binPath, "../cmd/traceport")
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build traceport binary with CGO_ENABLED=0: %v\n%s", err, string(out))
	}

	tpHome := t.TempDir()
	workDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, "service.go"), []byte("package service\nconst Version = 1\n"), 0644)

	runTP := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(binPath, args...)
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(), "TRACEPORT_HOME="+tpHome)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// 1. Verify ldflags version injection.
	verOut, err := runTP("version")
	if err != nil || strings.TrimSpace(verOut) != "traceport v1.1.0-e2e" {
		t.Fatalf("unexpected version output: %q (err=%v)", verOut, err)
	}

	// 2. Train workspace dictionary.
	dictOut, err := runTP("dict", "train")
	if err != nil || !strings.Contains(dictOut, "[ok] trained zstd dictionary") {
		t.Fatalf("dict train failed: %v\n%s", err, dictOut)
	}

	// 3. Record command that mutates workspace.
	var recArgs []string
	if runtime.GOOS == "windows" {
		recArgs = []string{"record", "--", "cmd.exe", "/c", "echo package service > generated.go && echo DONE_STEP"}
	} else {
		recArgs = []string{"record", "--", "sh", "-c", "printf 'package service\n' > generated.go && echo DONE_STEP"}
	}
	recOut, err := runTP(recArgs...)
	if err != nil {
		t.Fatalf("binary record failed: %v\n%s", err, recOut)
	}

	// 4. Verify integrity via CLI.
	verStoreOut, err := runTP("verify")
	if err != nil || !strings.Contains(verStoreOut, "[ok] verified 1 sessions") {
		t.Fatalf("binary verify failed: %v\n%s", err, verStoreOut)
	}
}
