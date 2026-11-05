package recorder

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

type Runner struct {
	store     *storage.Store
	workspace *checkpoint.WorkspaceManager
}

func NewRunner(store *storage.Store, workspaceDir string) *Runner {
	return &Runner{
		store:     store,
		workspace: checkpoint.NewWorkspaceManager(workspaceDir),
	}
}

func (r *Runner) Record(command string, args []string) (*model.Session, error) {
	sessionID := fmt.Sprintf("trace_%d", time.Now().UnixNano())
	fullCmd := command
	if len(args) > 0 {
		fullCmd = fmt.Sprintf("%s %s", command, fmt.Sprint(args))
	}

	session := &model.Session{
		ID:        sessionID,
		Command:   fullCmd,
		StartTime: time.Now(),
		Status:    "running",
		Frames:    []model.Frame{},
	}

	if err := r.store.SaveSession(session); err != nil {
		return nil, err
	}

	cmd := exec.Command(command, args...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	cmd.Stdin = os.Stdin

	// Initial workspace snapshot if in git
	if r.workspace.IsGitRepo() {
		if snap, err := r.workspace.CreateSnapshot(); err == nil {
			_ = r.store.AppendFrame(sessionID, model.Frame{
				Timestamp:   time.Now(),
				Type:        model.EventCheckpoint,
				Content:     "Initial flight snapshot",
				GitSnapshot: snap,
			})
		}
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Stream & capture stdout
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(io.TeeReader(stdoutPipe, os.Stdout))
		for scanner.Scan() {
			text := scanner.Text()
			_ = r.store.AppendFrame(sessionID, model.Frame{
				Timestamp: time.Now(),
				Type:      model.EventOutput,
				Content:   text,
			})
		}
	}()

	// Stream & capture stderr
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(io.TeeReader(stderrPipe, os.Stderr))
		for scanner.Scan() {
			text := scanner.Text()
			_ = r.store.AppendFrame(sessionID, model.Frame{
				Timestamp: time.Now(),
				Type:      model.EventError,
				Content:   text,
			})
		}
	}()

	cmdErr := cmd.Wait()
	wg.Wait()

	completedSession, err := r.store.GetSession(sessionID)
	if err != nil {
		return session, nil
	}

	completedSession.EndTime = time.Now()
	if cmdErr != nil {
		completedSession.Status = "aborted"
	} else {
		completedSession.Status = "completed"
	}

	_ = r.store.SaveSession(completedSession)
	return completedSession, cmdErr
}
