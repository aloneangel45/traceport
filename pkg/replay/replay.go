package replay

import (
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

// RewindOptions controls safety guards and dry-run behavior during workspace rewind.
type RewindOptions struct {
	Force  bool
	DryRun bool
}

// RewindResult reports the outcome of a RewindWithOptions operation.
type RewindResult struct {
	Session        *model.Session           `json:"session"`
	TargetStep     int                      `json:"target_step"`
	SnapshotHash   string                   `json:"snapshot_hash,omitempty"`
	SafetySnapshot string                   `json:"safety_snapshot,omitempty"`
	DryRun         bool                     `json:"dry_run"`
	Plan           *checkpoint.RestorePlan  `json:"plan,omitempty"`
	RestoreStats   *checkpoint.RestoreStats `json:"restore_stats,omitempty"`
}

// Engine executes time-travel rewinds, deterministic replays, diffs, verification, and GC.
type Engine struct {
	store *storage.Store
}

// NewEngine constructs a Replay & Time-Travel Engine backed by store.
func NewEngine(store *storage.Store) *Engine {
	return &Engine{store: store}
}

// Rewind rolls back workspaceDir and session state to targetStep using default safety guards.
func (e *Engine) Rewind(sessionID string, targetStep int, workspaceDir string) (*model.Session, error) {
	res, err := e.RewindWithOptions(sessionID, targetStep, workspaceDir, RewindOptions{})
	if err != nil {
		return nil, err
	}
	return res.Session, nil
}

// RewindWithOptions rolls back (or dry-run previews) workspaceDir and session timeline to targetStep,
// enforcing cross-workspace protection and creating an automatic pre-rewind safety savepoint.
func (e *Engine) RewindWithOptions(sessionID string, targetStep int, workspaceDir string, opts RewindOptions) (*RewindResult, error) {
	session, err := e.store.GetSession(sessionID)
	if err != nil {
		return nil, err
	}

	if targetStep < 1 || targetStep > len(session.Frames) {
		return nil, fmt.Errorf("target step %d out of bounds (1..%d)", targetStep, len(session.Frames))
	}

	absWork, err := filepath.Abs(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace path: %w", err)
	}

	// Prevent accidental cross-repository rewind unless --force is explicitly provided.
	if session.WorkspaceDir != "" && !opts.Force {
		if !sameWorkspacePath(session.WorkspaceDir, absWork) {
			return nil, fmt.Errorf(
				"refusing cross-workspace rewind: session recorded in %q, current workspace is %q (pass --force to override)",
				session.WorkspaceDir,
				absWork,
			)
		}
	}

	var snapshotHash string
	for i := targetStep - 1; i >= 0; i-- {
		if session.Frames[i].GitSnapshot != "" {
			snapshotHash = session.Frames[i].GitSnapshot
			break
		}
	}

	res := &RewindResult{
		Session:      session,
		TargetStep:   targetStep,
		SnapshotHash: snapshotHash,
		DryRun:       opts.DryRun,
	}

	treeStore, err := checkpoint.NewTreeStore(absWork, e.store.RootDir())
	if err != nil {
		return nil, fmt.Errorf("open shadow-tree store: %w", err)
	}
	defer treeStore.Close()

	if opts.DryRun {
		if snapshotHash != "" && treeStore.HasTree(snapshotHash) {
			plan, err := treeStore.PlanRestore(snapshotHash)
			if err != nil {
				return nil, fmt.Errorf("compute dry-run restore plan: %w", err)
			}
			res.Plan = plan
		}
		return res, nil
	}

	if snapshotHash != "" {
		// Capture automatic pre-rewind safety savepoint so uncommitted work is never lost.
		if safetyHash, sErr := treeStore.CreateSnapshot(); sErr == nil && safetyHash != "" {
			res.SafetySnapshot = safetyHash
			session.SafetySnapshot = safetyHash
		}

		if treeStore.HasTree(snapshotHash) {
			rStats, err := treeStore.RestoreSnapshotWithStats(snapshotHash)
			if err != nil {
				return nil, fmt.Errorf("failed to restore zstd workspace snapshot: %w", err)
			}
			res.RestoreStats = rStats
		} else {
			wm := checkpoint.NewWorkspaceManager(absWork)
			if checkpoint.IsValidGitHash(snapshotHash) && wm.IsGitRepo() {
				if err := wm.RestoreSnapshot(snapshotHash); err != nil {
					return nil, fmt.Errorf("failed to restore legacy git snapshot: %w", err)
				}
			} else {
				return nil, fmt.Errorf("snapshot %s not found in shadow-tree store or git repository", snapshotHash)
			}
		}
	}

	session.Frames = session.Frames[:targetStep]
	session.FrameCount = len(session.Frames)
	session.Status = fmt.Sprintf("rewound_to_step_%d", targetStep)
	session.EndTime = time.Time{}

	if err := e.store.SaveSession(session); err != nil {
		return nil, err
	}

	return res, nil
}

// Playback streams recorded session frames deterministically without executing child commands.
func (e *Engine) Playback(sessionID string, fromStep int, out io.Writer) error {
	session, err := e.store.GetSession(sessionID)
	if err != nil {
		return err
	}

	if fromStep < 1 {
		fromStep = 1
	}
	startIndex := fromStep - 1

	fmt.Fprintf(out, "=== Replaying Flight Session %s (from step %d) ===\n", session.ID, fromStep)

	for i := startIndex; i < len(session.Frames); i++ {
		frame := session.Frames[i]
		prefix := "OUTPUT"
		switch frame.Type {
		case model.EventError:
			prefix = "ERROR"
		case model.EventCheckpoint:
			prefix = "CHECKPOINT"
		case model.EventInput:
			prefix = "INPUT"
		case model.EventToolCall:
			prefix = "TOOL_CALL"
		case model.EventToolResult:
			prefix = "TOOL_RESULT"
		}

		fmt.Fprintf(out, "[Step %03d | %s] %s\n", frame.Index, prefix, frame.Content)
	}

	fmt.Fprintf(out, "=== End of Flight Replay ===\n")
	return nil
}

func sameWorkspacePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		absA = filepath.Clean(a)
		absB = filepath.Clean(b)
	}
	if evalA, err := filepath.EvalSymlinks(absA); err == nil {
		absA = evalA
	}
	if evalB, err := filepath.EvalSymlinks(absB); err == nil {
		absB = evalB
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(absA), filepath.Clean(absB))
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}
