package test

import (
	"bytes"
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

	// Record a real command execution
	session, err := runner.Record("cmd.exe", []string{"/c", "echo", "HELLO_TRACEPORT_TEST"})
	if err != nil {
		t.Fatalf("record failed: %v", err)
	}

	if session.Status != "completed" {
		t.Errorf("expected session completed, got %s", session.Status)
	}

	if len(session.Frames) == 0 {
		t.Errorf("expected at least 1 recorded frame")
	}

	// Verify replay engine can playback the flight
	eng := replay.NewEngine(store)
	var out bytes.Buffer
	if err := eng.Playback(session.ID, 1, &out); err != nil {
		t.Fatalf("playback failed: %v", err)
	}

	playbackOutput := out.String()
	if !bytes.Contains(out.Bytes(), []byte("HELLO_TRACEPORT_TEST")) {
		t.Errorf("expected replay to contain command output, got: %s", playbackOutput)
	}
}
