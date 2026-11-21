package recorder

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

const (
	// DefaultWatchInterval is the default polling cadence for traceport watch.
	DefaultWatchInterval = 2 * time.Second

	// DefaultWatchDebounce is the default quiet window used to coalesce burst edits.
	DefaultWatchDebounce = 500 * time.Millisecond

	// DefaultWatchMaxSnapshots caps the total checkpoint frames recorded in a single watch session.
	DefaultWatchMaxSnapshots = 200
)

// WatchOptions configures continuous workspace directory watching.
type WatchOptions struct {
	Dir          string
	Interval     time.Duration
	Debounce     time.Duration
	MaxSnapshots int
	Tags         []string
	Stdout       io.Writer
	Stderr       io.Writer
	OnSnapshot   func(model.Frame)
}

// Watch continuously monitors opts.Dir for workspace changes, coalescing burst edits over
// opts.Debounce and recording content-addressed shadow-tree checkpoint frames whenever the
// workspace tree hash changes. Respects .gitignore, .traceportignore, and default noisy
// directories (node_modules, .git, target, .traceport), enforces opts.MaxSnapshots, and
// shuts down gracefully when ctx is cancelled (marking the session "completed").
func Watch(ctx context.Context, store *storage.Store, opts WatchOptions) (*model.Session, error) {
	if store == nil {
		return nil, fmt.Errorf("nil storage store")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	dir := opts.Dir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determine working directory: %w", err)
		}
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve watch directory: %w", err)
	}
	info, err := os.Stat(absDir)
	if err != nil {
		return nil, fmt.Errorf("stat watch directory %q: %w", absDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("watch target %q is not a directory", absDir)
	}

	interval := opts.Interval
	if interval == 0 {
		interval = DefaultWatchInterval
	}
	if interval < 0 {
		return nil, fmt.Errorf("watch interval must be positive (got %s)", interval)
	}

	debounce := opts.Debounce
	if debounce < 0 {
		return nil, fmt.Errorf("watch debounce must be non-negative (got %s)", debounce)
	}

	maxSnapshots := opts.MaxSnapshots
	if maxSnapshots == 0 {
		maxSnapshots = DefaultWatchMaxSnapshots
	}
	if maxSnapshots < 0 {
		return nil, fmt.Errorf("max-snapshots must be positive (got %d)", maxSnapshots)
	}

	normTags, err := storage.NormalizeTags(opts.Tags)
	if err != nil {
		return nil, err
	}

	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	sessionID := nextUniqueSessionID()
	session := &model.Session{
		ID:           sessionID,
		Command:      "traceport watch",
		WorkspaceDir: absDir,
		Tags:         normTags,
		StartTime:    time.Now(),
		Status:       "running",
		Frames:       []model.Frame{},
	}
	if err := store.SaveSession(session); err != nil {
		return nil, fmt.Errorf("save watch session: %w", err)
	}

	treeStore, err := checkpoint.NewTreeStore(absDir, store.RootDir(), checkpoint.WithIgnoreFiltering())
	if err != nil {
		session.EndTime = time.Now()
		session.Status = "aborted"
		session.ExitCode = 1
		_ = store.UpdateSessionMeta(session)
		return nil, fmt.Errorf("initialize shadow-tree store: %w", err)
	}
	defer treeStore.Close()

	finalize := func() (*model.Session, error) {
		session.EndTime = time.Now()
		session.Status = "completed"
		session.ExitCode = 0
		if err := store.UpdateSessionMeta(session); err != nil {
			return session, err
		}
		loaded, err := store.GetSession(sessionID)
		if err != nil {
			return session, nil
		}
		return loaded, nil
	}

	var lastCommittedHash string
	var lastCommittedFP string
	snapshotsRecorded := 0

	recordCheckpoint := func(label string) (bool, error) {
		snap, err := treeStore.CreateSnapshot()
		if err != nil {
			return false, err
		}
		if snap == "" || snap == lastCommittedHash {
			return false, nil
		}
		frame, err := store.AppendFrameWithIndex(sessionID, model.Frame{
			Timestamp:   time.Now(),
			Type:        model.EventCheckpoint,
			Content:     label,
			GitSnapshot: snap,
		})
		if err != nil {
			return false, err
		}
		lastCommittedHash = snap
		snapshotsRecorded++
		shortHash := snap
		if len(shortHash) > 12 {
			shortHash = shortHash[:12]
		}
		fmt.Fprintf(stdout, "[step #%d] %s [tree:%s]\n", frame.Index, label, shortHash)
		if opts.OnSnapshot != nil {
			opts.OnSnapshot(frame)
		}
		return true, nil
	}

	fmt.Fprintf(
		stdout,
		"[traceport] watching %s (session=%s, interval=%s, debounce=%s, max_snapshots=%d)\n",
		absDir,
		sessionID,
		interval,
		debounce,
		maxSnapshots,
	)

	// Capture initial workspace state before entering the polling loop.
	if fp, fpErr := treeStore.WorkspaceFingerprint(); fpErr == nil {
		lastCommittedFP = fp
	}
	if _, err := recordCheckpoint("Initial workspace snapshot"); err != nil {
		session.EndTime = time.Now()
		session.Status = "aborted"
		session.ExitCode = 1
		_ = store.UpdateSessionMeta(session)
		return nil, fmt.Errorf("capture initial watch snapshot: %w", err)
	}
	// Refresh baseline fingerprint after initial snapshot.
	if fp, fpErr := treeStore.WorkspaceFingerprint(); fpErr == nil {
		lastCommittedFP = fp
	}

	if snapshotsRecorded >= maxSnapshots {
		fmt.Fprintf(stdout, "[traceport] reached max snapshots (%d); completing watch session %s\n", maxSnapshots, sessionID)
		return finalize()
	}

	pending := false
	pendingFP := ""
	var lastChangeAt time.Time

	nextWait := func() time.Duration {
		if !pending {
			return interval
		}
		if debounce == 0 {
			return 0
		}
		elapsed := time.Since(lastChangeAt)
		if elapsed >= debounce {
			return 0
		}
		remaining := debounce - elapsed
		step := interval
		if half := debounce / 2; half >= 10*time.Millisecond && half < step {
			step = half
		}
		if remaining < step {
			step = remaining
		}
		if step < 5*time.Millisecond {
			step = 5 * time.Millisecond
		}
		return step
	}

	flushFinalAndComplete := func() (*model.Session, error) {
		if snapshotsRecorded < maxSnapshots {
			needFinal := pending
			if !needFinal {
				if fp, fpErr := treeStore.WorkspaceFingerprint(); fpErr == nil && fp != lastCommittedFP {
					needFinal = true
				}
			}
			if needFinal {
				if _, err := recordCheckpoint("Final workspace snapshot"); err != nil {
					fmt.Fprintf(stderr, "[warn] final watch snapshot failed: %v\n", err)
				}
			}
		}
		fmt.Fprintf(stdout, "[ok] watch session completed: %s (%d snapshots)\n", sessionID, snapshotsRecorded)
		return finalize()
	}

	timer := time.NewTimer(nextWait())
	defer timer.Stop()

	for {
		if ctx.Err() != nil {
			return flushFinalAndComplete()
		}
		select {
		case <-ctx.Done():
			return flushFinalAndComplete()

		case <-timer.C:
			if ctx.Err() != nil {
				return flushFinalAndComplete()
			}
			fp, err := treeStore.WorkspaceFingerprint()
			if err != nil {
				fmt.Fprintf(stderr, "[warn] watch scan failed: %v\n", err)
				timer.Reset(interval)
				continue
			}

			now := time.Now()
			if !pending {
				if fp != lastCommittedFP {
					pending = true
					pendingFP = fp
					lastChangeAt = now
				}
			} else if fp != pendingFP {
				// Burst continues: extend quiet window from the latest observed mutation.
				pendingFP = fp
				lastChangeAt = now
			}

			if pending && (debounce == 0 || time.Since(lastChangeAt) >= debounce) {
				candidateFP := pendingFP
				pending = false
				pendingFP = ""
				label := fmt.Sprintf("Watch workspace checkpoint #%d", snapshotsRecorded+1)
				recorded, snapErr := recordCheckpoint(label)
				if snapErr != nil {
					// Keep lastCommittedFP unchanged so the next poll retries after transient locks.
					fmt.Fprintf(stderr, "[warn] watch checkpoint failed: %v\n", snapErr)
				} else if recorded {
					if postFP, fpErr := treeStore.WorkspaceFingerprint(); fpErr == nil {
						lastCommittedFP = postFP
					} else {
						lastCommittedFP = candidateFP
					}
					if snapshotsRecorded >= maxSnapshots {
						fmt.Fprintf(stdout, "[traceport] reached max snapshots (%d); completing watch session %s\n", maxSnapshots, sessionID)
						return finalize()
					}
				} else {
					// Tree hash unchanged (e.g. mtime-only touch or reverted edit); advance baseline FP.
					lastCommittedFP = candidateFP
				}
			}

			timer.Reset(nextWait())
		}
	}
}
