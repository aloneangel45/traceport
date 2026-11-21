package recorder

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

func TestWatch_DebounceCoalescesBurstEditsAndGracefulShutdown(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "sessions")
	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatalf("NewStoreWithPath: %v", err)
	}

	workDir := t.TempDir()
	appFile := filepath.Join(workDir, "app.go")
	if err := os.WriteFile(appFile, []byte("package main\nconst V = 1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var recorded []model.Frame
	snapCh := make(chan model.Frame, 16)

	var outBuf, errBuf bytes.Buffer
	doneCh := make(chan struct {
		sess *model.Session
		err  error
	}, 1)

	go func() {
		sess, wErr := Watch(ctx, store, WatchOptions{
			Dir:          workDir,
			Interval:     25 * time.Millisecond,
			Debounce:     160 * time.Millisecond,
			MaxSnapshots: 20,
			Tags:         []string{"ide-watch", "cursor"},
			Stdout:       &outBuf,
			Stderr:       &errBuf,
			OnSnapshot: func(f model.Frame) {
				mu.Lock()
				recorded = append(recorded, f)
				mu.Unlock()
				snapCh <- f
			},
		})
		doneCh <- struct {
			sess *model.Session
			err  error
		}{sess: sess, err: wErr}
	}()

	// Wait for initial snapshot (#1).
	select {
	case f := <-snapCh:
		if f.Index != 1 {
			t.Fatalf("expected initial frame #1, got #%d", f.Index)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for initial watch snapshot")
	}

	// Simulate rapid burst of 5 edits separated by 25ms (< 160ms debounce window).
	for i := 2; i <= 6; i++ {
		content := []byte("package main\nconst V = " + string(rune('0'+i)) + "\n")
		if err := os.WriteFile(appFile, content, 0644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Wait for coalesced snapshot (#2).
	select {
	case f := <-snapCh:
		if f.Index != 2 {
			t.Fatalf("expected coalesced frame #2, got #%d", f.Index)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for coalesced watch snapshot")
	}

	// Ensure no extra spurious frames appear during idle period.
	select {
	case extra := <-snapCh:
		t.Fatalf("expected burst to coalesce into a single snapshot, got extra frame #%d (%s)", extra.Index, extra.Content)
	case <-time.After(250 * time.Millisecond):
	}

	cancel()

	res := <-doneCh
	if res.err != nil {
		t.Fatalf("Watch returned error: %v", res.err)
	}
	if res.sess == nil {
		t.Fatalf("expected non-nil session")
	}
	if res.sess.Status != "completed" {
		t.Fatalf("expected session status 'completed', got %q", res.sess.Status)
	}
	if res.sess.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.sess.ExitCode)
	}
	if len(res.sess.Frames) != 2 {
		t.Fatalf("expected exactly 2 checkpoint frames (initial + 1 coalesced burst), got %d", len(res.sess.Frames))
	}
	if res.sess.Frames[0].GitSnapshot == res.sess.Frames[1].GitSnapshot {
		t.Fatalf("expected distinct tree hashes between initial and coalesced checkpoints")
	}
	if !storage.HasTag(*res.sess, "ide-watch") || !storage.HasTag(*res.sess, "cursor") {
		t.Fatalf("expected tags [ide-watch cursor], got %v", res.sess.Tags)
	}
}

func TestWatch_IgnoreRulesAndNoOpHashDedup(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "sessions")
	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatalf("NewStoreWithPath: %v", err)
	}

	workDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, ".gitignore"), []byte("*.log\ncache/\n"), 0644)
	_ = os.WriteFile(filepath.Join(workDir, ".traceportignore"), []byte("scratch.tmp\n"), 0644)
	_ = os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snapCh := make(chan model.Frame, 16)
	doneCh := make(chan *model.Session, 1)

	go func() {
		sess, _ := Watch(ctx, store, WatchOptions{
			Dir:          workDir,
			Interval:     25 * time.Millisecond,
			Debounce:     80 * time.Millisecond,
			MaxSnapshots: 20,
			OnSnapshot: func(f model.Frame) {
				snapCh <- f
			},
		})
		doneCh <- sess
	}()

	// Initial snapshot #1.
	select {
	case <-snapCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for initial snapshot")
	}

	// Mutate ONLY ignored directories and ignored files: node_modules, target, .git, .traceport, *.log, cache/, scratch.tmp.
	_ = os.MkdirAll(filepath.Join(workDir, "node_modules", "pkg"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, "node_modules", "pkg", "index.js"), []byte("console.log(1)"), 0644)
	_ = os.MkdirAll(filepath.Join(workDir, "target", "debug"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, "target", "debug", "app.o"), []byte("binary"), 0644)
	_ = os.MkdirAll(filepath.Join(workDir, ".git"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, ".git", "index.lock"), []byte("lock"), 0644)
	_ = os.MkdirAll(filepath.Join(workDir, "cache"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, "cache", "data.bin"), []byte("cached"), 0644)
	_ = os.WriteFile(filepath.Join(workDir, "debug.log"), []byte("noise"), 0644)
	_ = os.WriteFile(filepath.Join(workDir, "scratch.tmp"), []byte("temp"), 0644)

	select {
	case f := <-snapCh:
		t.Fatalf("ignored files must NOT trigger a checkpoint, got frame #%d", f.Index)
	case <-time.After(250 * time.Millisecond):
	}

	// Edit a tracked file and immediately revert to identical content -> tree hash unchanged -> no checkpoint.
	mainPath := filepath.Join(workDir, "main.go")
	orig, _ := os.ReadFile(mainPath)
	_ = os.WriteFile(mainPath, []byte("package main\n// temp\n"), 0644)
	_ = os.WriteFile(mainPath, orig, 0644)

	select {
	case f := <-snapCh:
		t.Fatalf("reverted identical content must NOT trigger a checkpoint, got frame #%d", f.Index)
	case <-time.After(250 * time.Millisecond):
	}

	// Now make a real tracked file change -> MUST trigger snapshot #2!
	_ = os.WriteFile(filepath.Join(workDir, "feature.go"), []byte("package main\nfunc Feature() {}\n"), 0644)
	select {
	case f := <-snapCh:
		if f.Index != 2 {
			t.Fatalf("expected frame #2 on real change, got #%d", f.Index)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for tracked file snapshot")
	}

	cancel()
	sess := <-doneCh
	if sess == nil || sess.Status != "completed" || len(sess.Frames) != 2 {
		t.Fatalf("unexpected final session state: %+v", sess)
	}
}

func TestWatch_MaxSnapshotsCap(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "sessions")
	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatalf("NewStoreWithPath: %v", err)
	}

	workDir := t.TempDir()
	target := filepath.Join(workDir, "counter.txt")
	_ = os.WriteFile(target, []byte("0\n"), 0644)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snapCh := make(chan model.Frame, 10)
	doneCh := make(chan struct {
		sess *model.Session
		err  error
	}, 1)

	go func() {
		sess, wErr := Watch(ctx, store, WatchOptions{
			Dir:          workDir,
			Interval:     20 * time.Millisecond,
			Debounce:     30 * time.Millisecond,
			MaxSnapshots: 3,
			OnSnapshot: func(f model.Frame) {
				snapCh <- f
			},
		})
		doneCh <- struct {
			sess *model.Session
			err  error
		}{sess: sess, err: wErr}
	}()

	// Wait for #1 (initial).
	<-snapCh
	_ = os.WriteFile(target, []byte("1\n"), 0644)
	// Wait for #2.
	<-snapCh
	_ = os.WriteFile(target, []byte("2\n"), 0644)
	// Wait for #3 (hits MaxSnapshots=3 and auto-completes).
	<-snapCh

	res := <-doneCh
	if res.err != nil {
		t.Fatalf("Watch failed: %v", res.err)
	}
	if res.sess.Status != "completed" {
		t.Fatalf("expected completed status at max snapshots, got %q", res.sess.Status)
	}
	if len(res.sess.Frames) != 3 {
		t.Fatalf("expected exactly 3 snapshots, got %d", len(res.sess.Frames))
	}
}

func TestWatch_ValidationErrors(t *testing.T) {
	store, err := storage.NewStoreWithPath(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	tmpFile := filepath.Join(t.TempDir(), "not_a_dir.txt")
	_ = os.WriteFile(tmpFile, []byte("hi"), 0644)

	cases := []WatchOptions{
		{Dir: filepath.Join(t.TempDir(), "does-not-exist")},
		{Dir: tmpFile},
		{Dir: t.TempDir(), Interval: -1 * time.Second},
		{Dir: t.TempDir(), Debounce: -1 * time.Millisecond},
		{Dir: t.TempDir(), MaxSnapshots: -5},
		{Dir: t.TempDir(), Tags: []string{"../bad"}},
	}
	for i, opts := range cases {
		if _, err := Watch(context.Background(), store, opts); err == nil {
			t.Fatalf("case %d: expected validation error for %+v", i, opts)
		}
	}
}
