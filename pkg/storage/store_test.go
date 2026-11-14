package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloneangel45/traceport/pkg/model"
)

func TestStore_SaveAndRetrieve(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStoreWithPath(tmpDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	session := &model.Session{
		ID:           "test_session_1",
		Command:      "go version",
		WorkspaceDir: tmpDir,
		StartTime:    time.Now(),
		Status:       "running",
		Frames:       []model.Frame{},
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("failed to save session: %v", err)
	}

	frame := model.Frame{
		Timestamp: time.Now(),
		Type:      model.EventOutput,
		Content:   "go version go1.25.0 windows/amd64",
	}

	if err := store.AppendFrame(session.ID, frame); err != nil {
		t.Fatalf("failed to append frame: %v", err)
	}

	retrieved, err := store.GetSession(session.ID)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}

	if len(retrieved.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(retrieved.Frames))
	}
	if retrieved.Frames[0].Index != 1 {
		t.Fatalf("expected frame index 1, got %d", retrieved.Frames[0].Index)
	}
	if retrieved.Frames[0].Content != frame.Content {
		t.Fatalf("content mismatch")
	}

	list, err := store.ListSessions()
	if err != nil {
		t.Fatalf("failed to list sessions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 session in list, got %d", len(list))
	}
}

func TestStore_AdversarialSessionIDValidation(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStoreWithPath(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	maliciousIDs := []string{
		"",
		".",
		"..",
		"../escape",
		"../../etc/passwd",
		`..\..\windows\win`,
		"trace/sub",
		"trace\\sub",
		"trace\x00null",
		"trace space",
		strings.Repeat("a", 129),
	}

	for _, badID := range maliciousIDs {
		if err := ValidateSessionID(badID); err == nil {
			t.Errorf("expected ValidateSessionID(%q) to fail", badID)
		}
		if err := store.SaveSession(&model.Session{ID: badID}); err == nil {
			t.Errorf("expected SaveSession(%q) to fail", badID)
		}
		if _, err := store.GetSession(badID); err == nil {
			t.Errorf("expected GetSession(%q) to fail", badID)
		}
		if err := store.AppendFrame(badID, model.Frame{Content: "x"}); err == nil {
			t.Errorf("expected AppendFrame(%q) to fail", badID)
		}
		if err := store.DeleteSession(badID); err == nil {
			t.Errorf("expected DeleteSession(%q) to fail", badID)
		}
	}
}

func TestStore_LegacyJSONBackwardCompatibilityAndMigration(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStoreWithPath(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	legacy := model.Session{
		ID:        "trace_legacy_v1",
		Command:   "python agent.py",
		StartTime: time.Now().Add(-time.Hour),
		Status:    "completed",
		Frames: []model.Frame{
			{Index: 1, Timestamp: time.Now(), Type: model.EventOutput, Content: "legacy step 1"},
		},
	}
	raw, _ := json.MarshalIndent(legacy, "", "  ")
	legacyPath := filepath.Join(tmpDir, "trace_legacy_v1.json")
	if err := os.WriteFile(legacyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetSession("trace_legacy_v1")
	if err != nil {
		t.Fatalf("GetSession on legacy JSON failed: %v", err)
	}
	if len(got.Frames) != 1 || got.Frames[0].Content != "legacy step 1" {
		t.Fatalf("unexpected legacy frames: %+v", got.Frames)
	}

	sessions, err := store.ListSessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("expected legacy session in ListSessions, got %d (err=%v)", len(sessions), err)
	}

	// Appending to legacy session migrates transparently to .meta.json + .frames.jsonl.
	if err := store.AppendFrame("trace_legacy_v1", model.Frame{
		Timestamp: time.Now(),
		Type:      model.EventOutput,
		Content:   "migrated step 2",
	}); err != nil {
		t.Fatalf("AppendFrame migration failed: %v", err)
	}

	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected legacy .json file to be removed after migration")
	}

	migrated, err := store.GetSession("trace_legacy_v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated.Frames) != 2 || migrated.Frames[1].Index != 2 || migrated.Frames[1].Content != "migrated step 2" {
		t.Fatalf("unexpected migrated frames: %+v", migrated.Frames)
	}
}

func TestStore_LargeFrameAndCrashRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStoreWithPath(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	sess := &model.Session{
		ID:        "trace_large",
		Command:   "llm-dump",
		StartTime: time.Now(),
		Status:    "running",
	}
	if err := store.SaveSession(sess); err != nil {
		t.Fatal(err)
	}

	// 1.5 MiB frame content (far exceeding 64 KiB Scanner limit).
	hugePayload := strings.Repeat("A", 1536*1024)
	if err := store.AppendFrame(sess.ID, model.Frame{
		Timestamp: time.Now(),
		Type:      model.EventOutput,
		Content:   hugePayload,
	}); err != nil {
		t.Fatalf("AppendFrame huge payload: %v", err)
	}

	// Simulate mid-write power loss by appending an unterminated partial JSON fragment.
	framesPath := filepath.Join(tmpDir, sess.ID+".frames.jsonl")
	f, err := os.OpenFile(framesPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"index":2,"type":"output","content":"truncated_mid_crash`)
	_ = f.Close()

	loaded, err := store.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession should recover gracefully from trailing partial line: %v", err)
	}
	if len(loaded.Frames) != 1 || len(loaded.Frames[0].Content) != len(hugePayload) {
		t.Fatalf("unexpected loaded frames count=%d len=%d", len(loaded.Frames), len(loaded.Frames[0].Content))
	}
}

func TestStore_ConcurrentAppends(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStoreWithPath(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	sess := &model.Session{
		ID:        "trace_concurrent",
		Command:   "parallel-agent",
		StartTime: time.Now(),
		Status:    "running",
	}
	if err := store.SaveSession(sess); err != nil {
		t.Fatal(err)
	}

	const workers = 16
	const perWorker = 25
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_ = store.AppendFrame(sess.ID, model.Frame{
					Timestamp: time.Now(),
					Type:      model.EventOutput,
					Content:   fmt.Sprintf("w=%d i=%d", id, i),
				})
			}
		}(w)
	}
	wg.Wait()

	got, err := store.GetSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantTotal := workers * perWorker
	if len(got.Frames) != wantTotal {
		t.Fatalf("expected %d frames, got %d", wantTotal, len(got.Frames))
	}
	for i, fr := range got.Frames {
		if fr.Index != i+1 {
			t.Fatalf("expected sequential frame index %d, got %d", i+1, fr.Index)
		}
	}
}

func TestResolveStoreDir(t *testing.T) {
	envDir := t.TempDir()
	t.Setenv("TRACEPORT_HOME", envDir)

	gotEnv := ResolveStoreDir(t.TempDir())
	wantEnv := filepath.Join(envDir, "sessions")
	if gotEnv != wantEnv {
		t.Fatalf("TRACEPORT_HOME override failed: got %q, want %q", gotEnv, wantEnv)
	}

	t.Setenv("TRACEPORT_HOME", "")
	repoRoot := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repoRoot, ".git"), 0755)
	nestedDir := filepath.Join(repoRoot, "pkg", "sub")
	_ = os.MkdirAll(nestedDir, 0755)

	gotGit := ResolveStoreDir(nestedDir)
	wantGit := filepath.Join(repoRoot, ".traceport", "sessions")
	if gotGit != wantGit {
		t.Fatalf("git root store discovery failed: got %q, want %q", gotGit, wantGit)
	}
}

func TestStore_TagValidationNormalizationAndLifecycle(t *testing.T) {
	norm, err := NormalizeTags([]string{"  Bug-42 ", "CI/Nightly,env:prod", "bug-42", "v1.2.0"})
	if err != nil {
		t.Fatalf("NormalizeTags failed: %v", err)
	}
	want := []string{"bug-42", "ci/nightly", "env:prod", "v1.2.0"}
	if len(norm) != len(want) {
		t.Fatalf("got %v, want %v", norm, want)
	}
	for i := range want {
		if norm[i] != want[i] {
			t.Fatalf("tag[%d] = %q, want %q", i, norm[i], want[i])
		}
	}

	invalidTags := []string{
		"",
		"   ",
		"-leading-dash",
		"../escape",
		"foo..bar",
		"has space",
		"bad$char",
		"trailing,",
		strings.Repeat("a", 65),
	}
	for _, bad := range invalidTags {
		if _, err := NormalizeTags([]string{bad}); err == nil {
			t.Errorf("expected NormalizeTags(%q) to fail", bad)
		}
	}

	tmpDir := t.TempDir()
	store, err := NewStoreWithPath(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	sess := &model.Session{
		ID:        "trace_tagged_1",
		Command:   "go test ./...",
		Tags:      []string{"Bug-42", "Regression"},
		StartTime: time.Now(),
		Status:    "completed",
		Frames: []model.Frame{
			{Index: 1, Timestamp: time.Now(), Type: model.EventOutput, Content: "PASS"},
		},
	}
	if err := store.SaveSession(sess); err != nil {
		t.Fatalf("SaveSession with tags: %v", err)
	}

	updated, err := store.AddSessionTags(sess.ID, []string{"ci,regression", "release-v1.2"})
	if err != nil {
		t.Fatalf("AddSessionTags failed: %v", err)
	}
	if !HasTag(*updated, "BUG-42") || !HasTag(*updated, "ci") || !HasTag(*updated, "release-v1.2") {
		t.Fatalf("unexpected tags after AddSessionTags: %v", updated.Tags)
	}
	if !MatchesAllTags(*updated, []string{"bug-42", "ci"}) {
		t.Fatalf("expected MatchesAllTags to match")
	}

	afterRm, err := store.RemoveSessionTags(sess.ID, []string{"REGRESSION"})
	if err != nil {
		t.Fatalf("RemoveSessionTags failed: %v", err)
	}
	if HasTag(*afterRm, "regression") || len(afterRm.Tags) != 3 {
		t.Fatalf("unexpected tags after RemoveSessionTags: %v", afterRm.Tags)
	}

	reloaded, err := store.GetSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Tags) != 3 || !HasTag(*reloaded, "release-v1.2") {
		t.Fatalf("persisted tags mismatch: %v", reloaded.Tags)
	}
}

func TestStore_CrashTruncatedJSONLFollowedByAppendFrame(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStoreWithPath(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	sess := &model.Session{
		ID:        "trace_crash_append",
		Command:   "agent run",
		StartTime: time.Now(),
		Status:    "running",
	}
	if err := store.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendFrame(sess.ID, model.Frame{
		Timestamp: time.Now(),
		Type:      model.EventOutput,
		Content:   "step_1_intact",
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate SIGKILL mid-line write (no trailing newline).
	framesPath := filepath.Join(tmpDir, sess.ID+".frames.jsonl")
	f, err := os.OpenFile(framesPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"index":2,"type":"output","content":"sigkill_partial`)
	_ = f.Close()

	// Subsequent AppendFrame MUST repair the partial tail before appending, rather than
	// concatenating onto the partial line and corrupting the JSONL log!
	reopened, err := NewStoreWithPath(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	appended, err := reopened.AppendFrameWithIndex(sess.ID, model.Frame{
		Timestamp: time.Now(),
		Type:      model.EventOutput,
		Content:   "step_2_clean_after_recovery",
	})
	if err != nil {
		t.Fatalf("AppendFrameWithIndex after crash failed: %v", err)
	}
	if appended.Index != 2 {
		t.Fatalf("expected repaired next frame index 2, got %d", appended.Index)
	}

	loaded, err := reopened.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession failed after post-crash append: %v", err)
	}
	if len(loaded.Frames) != 2 || loaded.Frames[0].Content != "step_1_intact" || loaded.Frames[1].Content != "step_2_clean_after_recovery" {
		t.Fatalf("unexpected frames after recovery + append: %+v", loaded.Frames)
	}

	// Also test valid JSON final line lacking trailing '\n' followed by AppendFrame.
	sess2 := &model.Session{
		ID:        "trace_no_nl_valid",
		Command:   "agent import",
		StartTime: time.Now(),
		Status:    "running",
	}
	if err := reopened.SaveSession(sess2); err != nil {
		t.Fatal(err)
	}
	noNLPath := filepath.Join(tmpDir, sess2.ID+".frames.jsonl")
	if err := os.WriteFile(noNLPath, []byte(`{"index":1,"timestamp":"2026-11-14T12:00:00Z","type":"output","content":"line_without_nl"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reopened.AppendFrame(sess2.ID, model.Frame{
		Timestamp: time.Now(),
		Type:      model.EventOutput,
		Content:   "second_line",
	}); err != nil {
		t.Fatalf("AppendFrame after valid no-newline frame failed: %v", err)
	}
	loaded2, err := reopened.GetSession(sess2.ID)
	if err != nil || len(loaded2.Frames) != 2 || loaded2.Frames[1].Index != 2 {
		t.Fatalf("expected 2 intact frames, got %+v (err=%v)", loaded2, err)
	}

	// Verify DeleteSession succeeds even if session files are marked read-only (0444).
	_ = os.Chmod(filepath.Join(tmpDir, sess2.ID+".meta.json"), 0444)
	_ = os.Chmod(noNLPath, 0444)
	if err := reopened.DeleteSession(sess2.ID); err != nil {
		t.Fatalf("DeleteSession failed on read-only session files: %v", err)
	}
}
