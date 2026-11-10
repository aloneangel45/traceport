package recorder

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

var lastSessionNano atomic.Int64

func nextUniqueSessionID() string {
	now := time.Now().UnixNano()
	for {
		prev := lastSessionNano.Load()
		next := now
		if next <= prev {
			next = prev + 1
		}
		if lastSessionNano.CompareAndSwap(prev, next) {
			return fmt.Sprintf("trace_%d", next)
		}
	}
}

// DefaultCheckpointInterval is the debounce interval for intermediate shadow-tree snapshots.
const DefaultCheckpointInterval = 40 * time.Millisecond

// Runner wraps child agent/CLI processes, capturing telemetry frames and Zstd shadow-tree savepoints.
type Runner struct {
	store              *storage.Store
	workspaceDir       string
	checkpointInterval time.Duration
	Stdin              io.Reader
	Stdout             io.Writer
	Stderr             io.Writer
}

// NewRunner creates a flight recorder Runner bound to workspaceDir and store.
func NewRunner(store *storage.Store, workspaceDir string) *Runner {
	absWork := workspaceDir
	if abs, err := filepath.Abs(workspaceDir); err == nil {
		absWork = abs
	}
	return &Runner{
		store:              store,
		workspaceDir:       absWork,
		checkpointInterval: DefaultCheckpointInterval,
		Stdin:              os.Stdin,
		Stdout:             os.Stdout,
		Stderr:             os.Stderr,
	}
}

// SetCheckpointInterval overrides the debounce interval for intermediate workspace snapshots.
func (r *Runner) SetCheckpointInterval(d time.Duration) {
	if d > 0 {
		r.checkpointInterval = d
	}
}

// Record spawns command with args inside workspaceDir, streaming stdout/stderr without line-size
// limits while capturing initial, intermediate, and final Zstandard shadow-tree checkpoints.
func (r *Runner) Record(command string, args []string) (*model.Session, error) {
	sessionID := nextUniqueSessionID()
	fullCmd := command
	if len(args) > 0 {
		fullCmd = command + " " + strings.Join(args, " ")
	}

	session := &model.Session{
		ID:           sessionID,
		Command:      fullCmd,
		WorkspaceDir: r.workspaceDir,
		StartTime:    time.Now(),
		Status:       "running",
		Frames:       []model.Frame{},
	}

	if err := r.store.SaveSession(session); err != nil {
		return nil, err
	}

	treeStore, err := checkpoint.NewTreeStore(r.workspaceDir, r.store.RootDir())
	if err != nil {
		return nil, fmt.Errorf("initialize shadow-tree store: %w", err)
	}
	defer treeStore.Close()

	var snapMu sync.Mutex
	var lastSnapHash string
	var lastCheckTime time.Time

	captureCheckpoint := func(label string, force bool) {
		snapMu.Lock()
		defer snapMu.Unlock()

		if !force && time.Since(lastCheckTime) < r.checkpointInterval {
			return
		}
		lastCheckTime = time.Now()

		snap, err := treeStore.CreateSnapshot()
		if err != nil || snap == "" || snap == lastSnapHash {
			return
		}
		lastSnapHash = snap
		_ = r.store.AppendFrame(sessionID, model.Frame{
			Timestamp:   time.Now(),
			Type:        model.EventCheckpoint,
			Content:     label,
			GitSnapshot: snap,
		})
	}

	// 1. Initial workspace snapshot (works in both Git and non-Git workspaces).
	captureCheckpoint("Initial workspace snapshot", true)

	cmd := exec.Command(command, args...)
	cmd.Dir = r.workspaceDir

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdin = r.Stdin

	if err := cmd.Start(); err != nil {
		session.EndTime = time.Now()
		session.Status = "aborted"
		session.ExitCode = 1
		_ = r.store.UpdateSessionMeta(session)
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	// Trap SIGINT / SIGTERM and forward cleanly to the child process.
	var interrupted atomic.Bool
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	doneCh := make(chan struct{})

	go func() {
		sigCount := 0
		for {
			select {
			case sig := <-sigCh:
				if sig == nil {
					return
				}
				interrupted.Store(true)
				sigCount++
				if cmd.Process != nil {
					if sigCount == 1 {
						if sErr := cmd.Process.Signal(sig); sErr != nil {
							_ = cmd.Process.Kill()
						}
					} else {
						_ = cmd.Process.Kill()
					}
				}
			case <-doneCh:
				return
			}
		}
	}()

	// Periodic debounced watcher captures intermediate workspace mutations during execution.
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		ticker := time.NewTicker(r.checkpointInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				captureCheckpoint("Intermediate workspace snapshot", true)
			case <-doneCh:
				return
			}
		}
	}()

	stdoutMirror := r.Stdout
	if stdoutMirror == nil {
		stdoutMirror = io.Discard
	}
	stderrMirror := r.Stderr
	if stderrMirror == nil {
		stderrMirror = io.Discard
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		streamPipeLines(io.TeeReader(stdoutPipe, stdoutMirror), func(line string) {
			captureCheckpoint("Intermediate workspace snapshot", false)
			_ = r.store.AppendFrame(sessionID, model.Frame{
				Timestamp: time.Now(),
				Type:      model.EventOutput,
				Content:   line,
			})
		})
	}()

	go func() {
		defer wg.Done()
		streamPipeLines(io.TeeReader(stderrPipe, stderrMirror), func(line string) {
			captureCheckpoint("Intermediate workspace snapshot", false)
			_ = r.store.AppendFrame(sessionID, model.Frame{
				Timestamp: time.Now(),
				Type:      model.EventError,
				Content:   line,
			})
		})
	}()

	wg.Wait()
	cmdErr := cmd.Wait()

	signal.Stop(sigCh)
	close(doneCh)
	<-watcherDone

	// Final post-execution workspace snapshot if any files mutated since the last checkpoint.
	captureCheckpoint("Final workspace snapshot", true)

	exitCode := 0
	if cmdErr != nil {
		var exitErr *exec.ExitError
		if errors.As(cmdErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	} else if interrupted.Load() {
		exitCode = 130
	}

	status := "completed"
	if cmdErr != nil || interrupted.Load() {
		status = "aborted"
	}

	session.EndTime = time.Now()
	session.ExitCode = exitCode
	session.Status = status
	_ = r.store.UpdateSessionMeta(session)

	completedSession, err := r.store.GetSession(sessionID)
	if err != nil {
		return session, cmdErr
	}
	return completedSession, cmdErr
}

// streamPipeLines reads newline-delimited output of arbitrary length using bufio.Reader
// so lines >64 KiB or >1 MiB never stall child stdout/stderr pipes.
func streamPipeLines(r io.Reader, onLine func(string)) {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			clean := strings.TrimSuffix(line, "\n")
			clean = strings.TrimSuffix(clean, "\r")
			onLine(clean)
		}
		if err != nil {
			break
		}
	}
}
