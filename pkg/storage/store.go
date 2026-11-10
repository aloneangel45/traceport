package storage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aloneangel45/traceport/pkg/model"
)

var validSessionIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateSessionID strictly validates session IDs to prevent path traversal.
func ValidateSessionID(id string) error {
	if len(id) == 0 || len(id) > 128 || !validSessionIDRe.MatchString(id) {
		return fmt.Errorf("invalid session ID %q: must match ^[a-zA-Z0-9_-]{1,128}$", id)
	}
	return nil
}

type sessionMeta struct {
	ID             string    `json:"id"`
	Command        string    `json:"command"`
	WorkspaceDir   string    `json:"workspace_dir,omitempty"`
	ExitCode       int       `json:"exit_code,omitempty"`
	SafetySnapshot string    `json:"safety_snapshot,omitempty"`
	StartTime      time.Time `json:"start_time"`
	EndTime        time.Time `json:"end_time,omitempty"`
	Status         string    `json:"status"`
	FrameCount     int       `json:"frame_count"`
}

// Store persists flight session metadata and append-only JSONL frame streams.
type Store struct {
	baseDir     string
	rootDir     string
	mu          sync.Mutex
	frameCounts map[string]int
}

// ResolveStoreDir resolves the session directory according to precedence:
// 1. TRACEPORT_HOME environment variable (<TRACEPORT_HOME>/sessions)
// 2. Nearest ancestor containing .traceport/ or .git (<root>/.traceport/sessions)
// 3. OS user config directory (<UserConfigDir>/TracePort/sessions)
func ResolveStoreDir(cwd string) string {
	if envHome := strings.TrimSpace(os.Getenv("TRACEPORT_HOME")); envHome != "" {
		return filepath.Join(envHome, "sessions")
	}

	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if absCwd, err := filepath.Abs(cwd); err == nil {
		cur := absCwd
		for {
			tpCandidate := filepath.Join(cur, ".traceport")
			if info, err := os.Stat(tpCandidate); err == nil && info.IsDir() {
				return filepath.Join(tpCandidate, "sessions")
			}
			gitCandidate := filepath.Join(cur, ".git")
			if _, err := os.Stat(gitCandidate); err == nil {
				return filepath.Join(cur, ".traceport", "sessions")
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}
	}

	if cfgDir, err := os.UserConfigDir(); err == nil && cfgDir != "" {
		return filepath.Join(cfgDir, "TracePort", "sessions")
	}
	if cwd != "" {
		return filepath.Join(cwd, ".traceport", "sessions")
	}
	return filepath.Join(".traceport", "sessions")
}

// NewStore creates a Store using unified workspace/environment directory discovery.
func NewStore() (*Store, error) {
	cwd, _ := os.Getwd()
	return NewStoreWithPath(ResolveStoreDir(cwd))
}

// NewStoreWithPath initializes a Store at an explicit directory path.
func NewStoreWithPath(dir string) (*Store, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absDir, 0700); err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(absDir, 0700)
	}

	rootDir := absDir
	if filepath.Base(absDir) == "sessions" {
		rootDir = filepath.Dir(absDir)
	}

	return &Store{
		baseDir:     absDir,
		rootDir:     rootDir,
		frameCounts: make(map[string]int),
	}, nil
}

// BaseDir returns the directory where session files are stored.
func (s *Store) BaseDir() string {
	return s.baseDir
}

// RootDir returns the root TracePort store directory containing blobs/, trees/, and dicts/.
func (s *Store) RootDir() string {
	return s.rootDir
}

func (s *Store) legacySessionFilePath(id string) string {
	return filepath.Join(s.baseDir, id+".json")
}

func (s *Store) metaFilePath(id string) string {
	return filepath.Join(s.baseDir, id+".meta.json")
}

func (s *Store) framesFilePath(id string) string {
	return filepath.Join(s.baseDir, id+".frames.jsonl")
}

// SaveSession atomically writes both session metadata and the full frame log.
func (s *Store) SaveSession(sess *model.Session) error {
	if sess == nil {
		return fmt.Errorf("nil session")
	}
	if err := ValidateSessionID(sess.ID); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saveFullSessionLocked(sess)
}

func (s *Store) saveFullSessionLocked(sess *model.Session) error {
	var buf bytes.Buffer
	for i := range sess.Frames {
		if sess.Frames[i].Index <= 0 {
			sess.Frames[i].Index = i + 1
		}
		line, err := json.Marshal(sess.Frames[i])
		if err != nil {
			return fmt.Errorf("marshal frame %d: %w", i+1, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	if err := writeAtomicFile(s.framesFilePath(sess.ID), buf.Bytes(), 0600); err != nil {
		return fmt.Errorf("write frames jsonl: %w", err)
	}

	s.frameCounts[sess.ID] = len(sess.Frames)
	sess.FrameCount = len(sess.Frames)

	if err := s.writeMetaLocked(sess); err != nil {
		return err
	}

	// Remove legacy monolithic <id>.json if migrating.
	_ = os.Remove(s.legacySessionFilePath(sess.ID))
	return nil
}

// UpdateSessionMeta atomically updates <id>.meta.json in O(1) without rewriting <id>.frames.jsonl.
func (s *Store) UpdateSessionMeta(sess *model.Session) error {
	if sess == nil {
		return fmt.Errorf("nil session")
	}
	if err := ValidateSessionID(sess.ID); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := os.Stat(s.metaFilePath(sess.ID)); os.IsNotExist(err) {
		if legacy, lErr := s.loadLegacySessionLocked(sess.ID); lErr == nil {
			legacy.Command = sess.Command
			legacy.WorkspaceDir = sess.WorkspaceDir
			legacy.ExitCode = sess.ExitCode
			legacy.SafetySnapshot = sess.SafetySnapshot
			legacy.EndTime = sess.EndTime
			legacy.Status = sess.Status
			return s.saveFullSessionLocked(legacy)
		}
	}

	count, ok := s.frameCounts[sess.ID]
	if !ok {
		frames, _ := readFramesJSONL(s.framesFilePath(sess.ID))
		count = len(frames)
		s.frameCounts[sess.ID] = count
	}
	sess.FrameCount = count
	return s.writeMetaLocked(sess)
}

func (s *Store) writeMetaLocked(sess *model.Session) error {
	meta := sessionMeta{
		ID:             sess.ID,
		Command:        sess.Command,
		WorkspaceDir:   sess.WorkspaceDir,
		ExitCode:       sess.ExitCode,
		SafetySnapshot: sess.SafetySnapshot,
		StartTime:      sess.StartTime,
		EndTime:        sess.EndTime,
		Status:         sess.Status,
		FrameCount:     sess.FrameCount,
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(s.metaFilePath(sess.ID), data, 0600)
}

// AppendFrame appends a single JSONL frame in O(1) time without rewriting existing frames.
func (s *Store) AppendFrame(sessionID string, frame model.Frame) error {
	_, err := s.AppendFrameWithIndex(sessionID, frame)
	return err
}

// AppendFrameWithIndex appends a single JSONL frame in O(1) and returns the indexed Frame.
func (s *Store) AppendFrameWithIndex(sessionID string, frame model.Frame) (model.Frame, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return frame, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	metaPath := s.metaFilePath(sessionID)
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		// Transparently migrate legacy <id>.json if present.
		legacy, lErr := s.loadLegacySessionLocked(sessionID)
		if lErr != nil {
			return frame, fmt.Errorf("session %s not found: %w", sessionID, err)
		}
		if err := s.saveFullSessionLocked(legacy); err != nil {
			return frame, err
		}
	}

	count, ok := s.frameCounts[sessionID]
	if !ok {
		existing, err := readFramesJSONL(s.framesFilePath(sessionID))
		if err != nil && !os.IsNotExist(err) {
			return frame, err
		}
		count = len(existing)
	}

	frame.Index = count + 1
	line, err := json.Marshal(frame)
	if err != nil {
		return frame, fmt.Errorf("marshal frame: %w", err)
	}
	line = append(line, '\n')

	f, err := os.OpenFile(s.framesFilePath(sessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return frame, fmt.Errorf("open frames log: %w", err)
	}
	_, writeErr := f.Write(line)
	closeErr := f.Close()
	if writeErr != nil {
		return frame, fmt.Errorf("append frame: %w", writeErr)
	}
	if closeErr != nil {
		return frame, fmt.Errorf("close frames log: %w", closeErr)
	}

	s.frameCounts[sessionID] = frame.Index
	return frame, nil
}

// GetSession loads a complete session and all recorded frames (supporting both JSONL and legacy v1.0 JSON).
func (s *Store) GetSession(sessionID string) (*model.Session, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.loadSessionLocked(sessionID)
}

func (s *Store) loadSessionLocked(sessionID string) (*model.Session, error) {
	metaPath := s.metaFilePath(sessionID)
	if metaBytes, err := os.ReadFile(metaPath); err == nil {
		var meta sessionMeta
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			return nil, fmt.Errorf("corrupt session metadata %s: %w", sessionID, err)
		}
		frames, err := readFramesJSONL(s.framesFilePath(sessionID))
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("corrupt session frames %s: %w", sessionID, err)
		}
		if frames == nil {
			frames = []model.Frame{}
		}
		s.frameCounts[sessionID] = len(frames)
		return &model.Session{
			ID:             meta.ID,
			Command:        meta.Command,
			WorkspaceDir:   meta.WorkspaceDir,
			ExitCode:       meta.ExitCode,
			SafetySnapshot: meta.SafetySnapshot,
			StartTime:      meta.StartTime,
			EndTime:        meta.EndTime,
			Status:         meta.Status,
			FrameCount:     len(frames),
			Frames:         frames,
		}, nil
	}

	return s.loadLegacySessionLocked(sessionID)
}

func (s *Store) loadLegacySessionLocked(sessionID string) (*model.Session, error) {
	path := s.legacySessionFilePath(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session %s not found: %w", sessionID, err)
	}

	var sess model.Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("corrupt session %s: %w", sessionID, err)
	}
	if sess.Frames == nil {
		sess.Frames = []model.Frame{}
	}
	sess.FrameCount = len(sess.Frames)
	return &sess, nil
}

// ListSessions returns all recorded sessions sorted newest StartTime first.
func (s *Store) ListSessions() ([]model.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var ids []string

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".meta.json") {
			id := strings.TrimSuffix(name, ".meta.json")
			if ValidateSessionID(id) == nil && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".meta.json") && name != "stat_cache.json" {
			id := strings.TrimSuffix(name, ".json")
			if ValidateSessionID(id) == nil && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}

	sessions := make([]model.Session, 0, len(ids))
	for _, id := range ids {
		if sess, err := s.loadSessionLocked(id); err == nil {
			sessions = append(sessions, *sess)
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartTime.After(sessions[j].StartTime)
	})
	return sessions, nil
}

// DeleteSession removes a session's metadata, JSONL frames, and any legacy JSON file.
func (s *Store) DeleteSession(sessionID string) error {
	if err := ValidateSessionID(sessionID); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.frameCounts, sessionID)
	var firstErr error
	for _, p := range []string{
		s.metaFilePath(sessionID),
		s.framesFilePath(sessionID),
		s.legacySessionFilePath(sessionID),
	} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// readFramesJSONL streams arbitrarily long JSONL lines via bufio.Reader (no 64 KiB Scanner cap).
// If the final line lacks a trailing newline AND fails JSON decoding (crash mid-write), it is ignored.
func readFramesJSONL(path string) ([]model.Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64*1024)
	var frames []model.Frame
	lineNum := 0

	for {
		line, readErr := reader.ReadBytes('\n')
		lineNum++
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var frame model.Frame
			if uErr := json.Unmarshal(trimmed, &frame); uErr != nil {
				// Tolerate crash-truncated final partial line lacking newline terminator.
				if readErr == io.EOF && !bytes.HasSuffix(line, []byte("\n")) {
					break
				}
				return nil, fmt.Errorf("line %d: %w", lineNum, uErr)
			}
			frames = append(frames, frame)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}

	return frames, nil
}

func writeAtomicFile(dest string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".tp-meta-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(perm); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Chmod(dest, 0600)
		if retryErr := os.Rename(tmpName, dest); retryErr != nil {
			return retryErr
		}
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dest, perm)
	}
	cleanup = false
	return nil
}
