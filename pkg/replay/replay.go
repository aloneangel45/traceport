package replay

import (
	"fmt"
	"io"
	"time"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

type Engine struct {
	store *storage.Store
}

func NewEngine(store *storage.Store) *Engine {
	return &Engine{store: store}
}

func (e *Engine) Rewind(sessionID string, targetStep int, workspaceDir string) (*model.Session, error) {
	session, err := e.store.GetSession(sessionID)
	if err != nil {
		return nil, err
	}

	if targetStep < 1 || targetStep > len(session.Frames) {
		return nil, fmt.Errorf("target step %d out of bounds (1..%d)", targetStep, len(session.Frames))
	}

	// Find the most recent snapshot at or before targetStep
	var snapshotHash string
	for i := targetStep - 1; i >= 0; i-- {
		if session.Frames[i].GitSnapshot != "" {
			snapshotHash = session.Frames[i].GitSnapshot
			break
		}
	}

	// Restore workspace if snapshot found
	if snapshotHash != "" {
		wm := checkpoint.NewWorkspaceManager(workspaceDir)
		if wm.IsGitRepo() {
			if err := wm.RestoreSnapshot(snapshotHash); err != nil {
				return nil, fmt.Errorf("failed to restore workspace snapshot: %w", err)
			}
		}
	}

	// Fork/rewind session state
	session.Frames = session.Frames[:targetStep]
	session.Status = fmt.Sprintf("rewound_to_step_%d", targetStep)
	session.EndTime = time.Time{}

	if err := e.store.SaveSession(session); err != nil {
		return nil, err
	}

	return session, nil
}

func (e *Engine) Playback(sessionID string, fromStep int, out io.Writer) error {
	session, err := e.store.GetSession(sessionID)
	if err != nil {
		return err
	}

	startIndex := fromStep - 1
	if startIndex < 0 {
		startIndex = 0
	}

	fmt.Fprintf(out, "=== Replaying Flight Session %s (from step %d) ===\n", session.ID, fromStep)

	for i := startIndex; i < len(session.Frames); i++ {
		frame := session.Frames[i]
		prefix := "OUTPUT"
		if frame.Type == model.EventError {
			prefix = "ERROR"
		} else if frame.Type == model.EventCheckpoint {
			prefix = "CHECKPOINT"
		}

		fmt.Fprintf(out, "[Step %03d | %s] %s\n", frame.Index, prefix, frame.Content)
	}

	fmt.Fprintf(out, "=== End of Flight Replay ===\n")
	return nil
}
