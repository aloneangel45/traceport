package replay

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestEngine_ExportAndSearch(t *testing.T) {
	workDir := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), "sessions")

	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	sess, _, _ := buildMultiStepTestSession(t, store, workDir, "trace_export_1")
	sess.Tags = []string{"incident-7", "regression"}
	sess.EndTime = sess.StartTime.Add(250 * time.Millisecond)
	_ = store.UpdateSessionMeta(sess)

	slackFake := "xox" + "b-111222333-444555666-secretSlackVal"
	awsFake := "AKIA" + "IOSFODNN7EXAMPLE"

	// Append a tool frame with a Slack token to verify tool redaction & tool search.
	_ = store.AppendFrame(sess.ID, model.Frame{
		Timestamp: time.Now(),
		Type:      model.EventToolCall,
		Content:   "notify slack channel",
		Tool: &model.ToolInvocation{
			Name:       "slack_post",
			Arguments:  map[string]interface{}{"webhook_token": slackFake},
			Result:     "posted with " + awsFake,
			DurationMs: 18,
		},
	})

	eng := NewEngine(store)

	// 1. Unsupported export format must fail cleanly.
	if _, err := eng.ExportSession(sess.ID, ExportOptions{Format: "xml"}); err == nil {
		t.Fatalf("expected error for unsupported export format")
	}

	// 2. Markdown export with redaction must include metadata, checkpoint delta, and scrub secrets.
	mdBytes, err := eng.ExportSession(sess.ID, ExportOptions{Format: "md", Redact: true})
	if err != nil {
		t.Fatalf("ExportSession(md, redact) failed: %v", err)
	}
	md := string(mdBytes)
	for _, want := range []string{"incident-7, regression", "helper.go", "obsolete.txt", "slack_post", "[REDACTED]"} {
		if !strings.Contains(md, want) {
			t.Fatalf("expected %q in markdown export:\n%s", want, md)
		}
	}
	for _, secret := range []string{slackFake, awsFake} {
		if strings.Contains(md, secret) {
			t.Fatalf("secret %q leaked in markdown export", secret)
		}
	}

	// 3. Search invalid regex must return error.
	if _, err := eng.Search(SearchOptions{Query: "[unclosed", Regex: true}); err == nil {
		t.Fatalf("expected error on invalid regex")
	}

	// 4. Search deleted checkpoint file path ("obsolete.txt") & tool invocation ("slack_post").
	delRes, err := eng.Search(SearchOptions{Query: "obsolete.txt", Tags: []string{"incident-7"}})
	if err != nil || len(delRes) != 1 || !strings.Contains(delRes[0].Matches[0].Snippet, "deleted obsolete.txt") {
		t.Fatalf("expected deleted obsolete.txt checkpoint match, got %+v (err=%v)", delRes, err)
	}

	toolRes, err := eng.Search(SearchOptions{Query: "slack_post"})
	if err != nil || len(toolRes) != 1 {
		t.Fatalf("expected tool search match for slack_post, got %+v (err=%v)", toolRes, err)
	}
}

func TestEngine_RefusesGCWhenSessionOrManifestCorrupt(t *testing.T) {
	workDir := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), "sessions")

	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	sess, snap1, _ := buildMultiStepTestSession(t, store, workDir, "trace_gc_safe")
	eng := NewEngine(store)

	// Introduce a second session with corrupt .meta.json on disk.
	corruptMeta := filepath.Join(store.BaseDir(), "trace_corrupt_99.meta.json")
	if err := os.WriteFile(corruptMeta, []byte("{corrupt-json"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.GC(GCOptions{Keep: -1}); err == nil {
		t.Fatalf("expected GC to refuse sweeping when an unreadable session exists on disk")
	}
	_ = os.Remove(corruptMeta)

	// Corrupt snap1 manifest and verify GC refuses to sweep snap1's blobs!
	manifestFile := filepath.Join(store.RootDir(), "trees", snap1+".manifest.zst")
	origManifest, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestFile, []byte("corrupt_manifest_bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.GC(GCOptions{Keep: -1}); err == nil {
		t.Fatalf("expected GC to abort when a retained manifest fails to load")
	}

	// Restore clean manifest and verify session still verifies 100% (no blobs were lost!).
	if err := os.WriteFile(manifestFile, origManifest, 0600); err != nil {
		t.Fatal(err)
	}
	vRep, err := eng.Verify(sess.ID)
	if err != nil || !vRep.Valid {
		t.Fatalf("blobs lost during aborted GC! verify=%+v err=%v", vRep, err)
	}
}

func TestEngine_LiveDiffStorePrefixCollisionAndStepBounds(t *testing.T) {
	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".traceport", "sessions")
	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	// Files starting with ".traceport" prefix (NOT inside .traceport/) MUST NOT be skipped by live diff!
	prefixFile := filepath.Join(workDir, ".traceport-ci.yaml")
	prefixNested := filepath.Join(workDir, ".traceport_docs", "guide.md")
	_ = os.MkdirAll(filepath.Dir(prefixNested), 0755)
	_ = os.WriteFile(prefixFile, []byte("ci: true\n"), 0644)
	_ = os.WriteFile(prefixNested, []byte("# Guide\n"), 0644)

	ts, err := checkpoint.NewTreeStore(workDir, store.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	_ = ts.Close()

	sess := &model.Session{
		ID:           "trace_prefix_diff",
		Command:      "go test",
		WorkspaceDir: workDir,
		StartTime:    time.Now(),
		Status:       "completed",
		Frames: []model.Frame{
			{Index: 1, Timestamp: time.Now(), Type: model.EventCheckpoint, Content: "Initial", GitSnapshot: snap},
		},
	}
	if err := store.SaveSession(sess); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(store)
	diffRep, err := eng.DiffSession(sess.ID, 1, 0, workDir)
	if err != nil {
		t.Fatalf("DiffSession live failed: %v", err)
	}
	if len(diffRep.Changes) != 0 {
		t.Fatalf("expected 0 live diff changes for unchanged .traceport-* files, got %+v", diffRep.Changes)
	}

	// Negative step indices and out-of-bounds Playback must return errors.
	if _, err := eng.DiffSession(sess.ID, -1, 0, workDir); err == nil {
		t.Fatalf("expected error for negative fromStep")
	}
	if _, err := eng.DiffSession(sess.ID, 1, -2, workDir); err == nil {
		t.Fatalf("expected error for negative toStep")
	}
	var pb bytes.Buffer
	if err := eng.Playback(sess.ID, 99, &pb); err == nil {
		t.Fatalf("expected error for out-of-bounds Playback fromStep")
	}

	// DryRun rewind with missing snapshot must fail just like live rewind.
	missingSess := &model.Session{
		ID:           "trace_missing_snap",
		Command:      "agent",
		WorkspaceDir: workDir,
		StartTime:    time.Now(),
		Status:       "aborted",
		Frames: []model.Frame{
			{
				Index:       1,
				Timestamp:   time.Now(),
				Type:        model.EventCheckpoint,
				Content:     "Missing",
				GitSnapshot: strings.Repeat("f", 64),
			},
		},
	}
	_ = store.SaveSession(missingSess)
	if _, err := eng.RewindWithOptions(missingSess.ID, 1, workDir, RewindOptions{DryRun: true}); err == nil {
		t.Fatalf("expected DryRun rewind to fail when snapshot manifest does not exist")
	}
}

func TestEngine_ExportMarkdownUTF8BoundaryAndTildeFenceEscape(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "sessions")
	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	// Build >2000-rune multi-byte UTF-8 content containing 5-tilde fence "~~~~~".
	multiByteChunk := "Çağdaş日本語🚀"
	longContent := "~~~~~\ninjected fence\n~~~~~\n" + strings.Repeat(multiByteChunk, 260)
	longResult := strings.Repeat("şğüöçı漢字", 160)

	sess := &model.Session{
		ID:        "trace_utf8_md",
		Command:   "echo utf8",
		StartTime: time.Now(),
		Status:    "completed",
		Frames: []model.Frame{
			{
				Index:     1,
				Timestamp: time.Now(),
				Type:      model.EventToolCall,
				Content:   longContent,
				Tool: &model.ToolInvocation{
					Name:   "utf8_tool",
					Result: longResult,
				},
			},
		},
	}
	if err := store.SaveSession(sess); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(store)
	mdBytes, err := eng.ExportSession(sess.ID, ExportOptions{Format: "markdown"})
	if err != nil {
		t.Fatalf("ExportSession markdown failed: %v", err)
	}
	if !utf8.Valid(mdBytes) {
		t.Fatalf("ExportSession markdown produced invalid UTF-8 bytes on multi-byte truncation boundary!")
	}
	if !strings.Contains(string(mdBytes), "~~~~~~text\n") {
		t.Fatalf("expected 6-tilde outer fence to escape embedded 5-tilde sequence:\n%s", string(mdBytes)[:400])
	}
}
