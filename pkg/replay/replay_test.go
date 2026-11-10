package replay

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

func buildMultiStepTestSession(t *testing.T, store *storage.Store, workDir, id string) (*model.Session, string, string) {
	t.Helper()
	ts, err := checkpoint.NewTreeStore(workDir, store.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	// Step 1 state
	_ = os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package main\n\nfunc main() {\n\tprintln(\"step1\")\n}\n"), 0644)
	_ = os.WriteFile(filepath.Join(workDir, "obsolete.txt"), []byte("remove me later\n"), 0644)
	snap1, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	// Step 3 state (intermediate)
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package main\n\nfunc main() {\n\tprintln(\"step2_good\")\n}\n"), 0644)
	_ = os.WriteFile(filepath.Join(workDir, "helper.go"), []byte("package main\n\nfunc helper() {}\n"), 0644)
	_ = os.Remove(filepath.Join(workDir, "obsolete.txt"))
	snap2, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	// Step 5 state (broken final)
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package main\n\nBROKEN_SYNTAX\n"), 0644)
	_ = os.WriteFile(filepath.Join(workDir, "crash.dump"), []byte{0x00, 0xFF, 0xFE, 0x01}, 0644)
	snap3, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	sess := &model.Session{
		ID:           id,
		Command:      "agent run",
		WorkspaceDir: workDir,
		StartTime:    time.Now(),
		Status:       "aborted",
		ExitCode:     1,
		Frames: []model.Frame{
			{Index: 1, Timestamp: time.Now(), Type: model.EventCheckpoint, Content: "Initial workspace snapshot", GitSnapshot: snap1},
			{Index: 2, Timestamp: time.Now(), Type: model.EventOutput, Content: "edited helper.go"},
			{Index: 3, Timestamp: time.Now(), Type: model.EventCheckpoint, Content: "Intermediate workspace snapshot", GitSnapshot: snap2},
			{Index: 4, Timestamp: time.Now(), Type: model.EventError, Content: "broken syntax"},
			{Index: 5, Timestamp: time.Now(), Type: model.EventCheckpoint, Content: "Final workspace snapshot", GitSnapshot: snap3},
		},
	}
	if err := store.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	return sess, snap1, snap2
}

func TestEngine_RewindGuardsDryRunAndSafetySavepoint(t *testing.T) {
	workDir := t.TempDir()
	otherDir := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), "sessions")

	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	sess, _, _ := buildMultiStepTestSession(t, store, workDir, "trace_rewind_1")

	eng := NewEngine(store)

	// 1. Cross-workspace guard must block rewind into otherDir unless Force=true.
	if _, err := eng.Rewind(sess.ID, 3, otherDir); err == nil || !strings.Contains(err.Error(), "cross-workspace") {
		t.Fatalf("expected cross-workspace guard error, got: %v", err)
	}

	// 2. Add an uncommitted manual user file in workDir before rewind to verify SafetySnapshot preserves it!
	uncommittedFile := filepath.Join(workDir, "my_uncommitted_notes.md")
	_ = os.WriteFile(uncommittedFile, []byte("precious manual notes\n"), 0644)

	// 3. Dry-run must preview changes without deleting uncommitted file or modifying main.go.
	dryRes, err := eng.RewindWithOptions(sess.ID, 3, workDir, RewindOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run rewind failed: %v", err)
	}
	if dryRes.Plan == nil || len(dryRes.Plan.Modify) != 1 || len(dryRes.Plan.Delete) != 2 {
		t.Fatalf("unexpected dry-run plan: %+v", dryRes.Plan)
	}
	if _, err := os.Stat(uncommittedFile); err != nil {
		t.Fatalf("dry-run must not delete uncommitted file!")
	}

	// 4. Execute live rewind to intermediate step 3.
	liveRes, err := eng.RewindWithOptions(sess.ID, 3, workDir, RewindOptions{})
	if err != nil {
		t.Fatalf("live rewind failed: %v", err)
	}
	if liveRes.SafetySnapshot == "" {
		t.Fatalf("expected automatic pre-rewind safety savepoint hash")
	}

	mainData, _ := os.ReadFile(filepath.Join(workDir, "main.go"))
	if !strings.Contains(string(mainData), "step2_good") {
		t.Fatalf("expected main.go restored to step 3 state, got: %s", string(mainData))
	}
	if _, err := os.Stat(filepath.Join(workDir, "crash.dump")); !os.IsNotExist(err) {
		t.Fatalf("expected post-step-3 crash.dump to be removed")
	}

	// 5. Verify pre-rewind safety savepoint can recover uncommitted manual notes!
	ts, err := checkpoint.NewTreeStore(workDir, store.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if err := ts.RestoreSnapshot(liveRes.SafetySnapshot); err != nil {
		t.Fatalf("restore from SafetySnapshot failed: %v", err)
	}
	recoveredNotes, err := os.ReadFile(uncommittedFile)
	if err != nil || string(recoveredNotes) != "precious manual notes\n" {
		t.Fatalf("failed to recover uncommitted work from safety savepoint: %v", err)
	}
}

func TestEngine_DiffVerifyAndGC(t *testing.T) {
	workDir := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), "sessions")

	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	// Create an older session and a newer session.
	oldSess, _, _ := buildMultiStepTestSession(t, store, workDir, "trace_old")
	oldSess.StartTime = time.Now().Add(-2 * time.Hour)
	_ = store.UpdateSessionMeta(oldSess)

	newWorkDir := t.TempDir()
	newSess, _, _ := buildMultiStepTestSession(t, store, newWorkDir, "trace_new")

	eng := NewEngine(store)

	// 1. Test DiffSession between step 1 and step 3.
	diff13, err := eng.DiffSession(newSess.ID, 1, 3, newWorkDir)
	if err != nil {
		t.Fatalf("DiffSession(1..3) failed: %v", err)
	}
	if diff13.FilesAdded != 1 || diff13.FilesMod != 1 || diff13.FilesDel != 1 {
		t.Fatalf("unexpected diff counts: %+v", diff13)
	}
	if !strings.Contains(diff13.UnifiedText, "-	println(\"step1\")") ||
		!strings.Contains(diff13.UnifiedText, "+	println(\"step2_good\")") {
		t.Fatalf("expected unified line diff in output:\n%s", diff13.UnifiedText)
	}

	// Test DiffSession against binary file in step 5 / live working tree.
	diffLive, err := eng.DiffSession(newSess.ID, 3, 0, newWorkDir)
	if err != nil {
		t.Fatalf("DiffSession(3..live) failed: %v", err)
	}
	if !strings.Contains(diffLive.UnifiedText, "Binary files /dev/null and b/crash.dump differ") {
		t.Fatalf("expected binary diff marker:\n%s", diffLive.UnifiedText)
	}

	// 2. Test Verify on clean store.
	vRep, err := eng.Verify("")
	if err != nil {
		t.Fatalf("Verify clean store failed: %v", err)
	}
	if !vRep.Valid || vRep.SessionsChecked != 2 || vRep.TreesVerified < 3 {
		t.Fatalf("unexpected verify report: %+v", vRep)
	}

	// 3. Test GC dry-run then live GC keeping 1 session.
	// Add an orphan unreferenced blob to verify sweep.
	orphanDir := filepath.Join(store.RootDir(), "blobs", "ff")
	_ = os.MkdirAll(orphanDir, 0700)
	_ = os.WriteFile(filepath.Join(orphanDir, strings.Repeat("0", 62)+".zst"), []byte("orphan"), 0600)

	dryGC, err := eng.GC(GCOptions{Keep: 1, DryRun: true})
	if err != nil {
		t.Fatalf("GC dry-run: %v", err)
	}
	if dryGC.SessionsPruned != 1 || dryGC.BlobsRemoved < 1 || dryGC.BytesReclaimed == 0 {
		t.Fatalf("unexpected dry-run GC report: %+v", dryGC)
	}

	liveGC, err := eng.GC(GCOptions{Keep: 1, DryRun: false})
	if err != nil {
		t.Fatalf("GC live: %v", err)
	}
	if liveGC.SessionsRetained != 1 || liveGC.SessionsPruned != 1 {
		t.Fatalf("unexpected live GC report: %+v", liveGC)
	}

	// Retained session must still verify 100% cleanly after GC!
	vAfter, err := eng.Verify(newSess.ID)
	if err != nil || !vAfter.Valid {
		t.Fatalf("retained session failed verification after GC: %+v (err=%v)", vAfter, err)
	}

	// Playback check
	var pb bytes.Buffer
	if err := eng.Playback(newSess.ID, 2, &pb); err != nil {
		t.Fatalf("Playback failed: %v", err)
	}
	if !strings.Contains(pb.String(), "edited helper.go") {
		t.Fatalf("unexpected playback output: %s", pb.String())
	}
}
