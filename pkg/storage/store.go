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

var (
	validSessionIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	validTagRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,63}$`)
)

// ValidateSessionID strictly validates session IDs to prevent path traversal.
func ValidateSessionID(id string) error {
	if len(id) == 0 || len(id) > 128 || !validSessionIDRe.MatchString(id) {
		return fmt.Errorf("invalid session ID %q: must match ^[a-zA-Z0-9_-]{1,128}$", id)
	}
	return nil
}

// NormalizeTag trims, lowercases, and validates a single tag string.
func NormalizeTag(raw string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(raw))
	if t == "" {
		return "", fmt.Errorf("invalid empty tag")
	}
	if len(t) > 64 || strings.Contains(t, "..") || !validTagRe.MatchString(t) {
		return "", fmt.Errorf("invalid tag %q: must match ^[a-z0-9][a-z0-9._:/-]{0,63}$ without '..'", raw)
	}
	return t, nil
}

// ValidateTag verifies that raw is a safe, non-empty tag value.
func ValidateTag(raw string) error {
	_, err := NormalizeTag(raw)
	return err
}

// NormalizeTags splits comma-separated tag inputs, trims, lowercases, validates, and deduplicates them.
func NormalizeTags(inputs []string) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(inputs))
	out := make([]string, 0, len(inputs))
	for _, raw := range inputs {
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("invalid empty tag")
		}
		parts := strings.Split(raw, ",")
		for _, part := range parts {
			norm, err := NormalizeTag(part)
			if err != nil {
				return nil, err
			}
			if _, exists := seen[norm]; !exists {
				seen[norm] = struct{}{}
				out = append(out, norm)
			}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// HasTag reports whether sess carries targetTag (after normalization).
func HasTag(sess model.Session, targetTag string) bool {
	want, err := NormalizeTag(targetTag)
	if err != nil {
		return false
	}
	for _, existing := range sess.Tags {
		if norm, nErr := NormalizeTag(existing); nErr == nil && norm == want {
			return true
		}
	}
	return false
}

// MatchesAllTags reports whether sess carries every tag in filterTags.
func MatchesAllTags(sess model.Session, filterTags []string) bool {
	for _, t := range filterTags {
		if !HasTag(sess, t) {
			return false
		}
	}
	return true
}

type sessionMeta struct {
	ID             string    `json:"id"`
	Command        string    `json:"command"`
	WorkspaceDir   string    `json:"workspace_dir,omitempty"`
	Tags           []string  `json:"tags,omitempty"`
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
	normTags, err := NormalizeTags(sess.Tags)
	if err != nil {
		return err
	}
	sess.Tags = normTags

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
	normTags, err := NormalizeTags(sess.Tags)
	if err != nil {
		return err
	}
	sess.Tags = normTags

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := os.Stat(s.metaFilePath(sess.ID)); os.IsNotExist(err) {
		if legacy, lErr := s.loadLegacySessionLocked(sess.ID); lErr == nil {
			legacy.Command = sess.Command
			legacy.WorkspaceDir = sess.WorkspaceDir
			legacy.Tags = sess.Tags
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

// AddSessionTags appends normalized, deduplicated tags to an existing session.
func (s *Store) AddSessionTags(sessionID string, tags []string) (*model.Session, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return nil, err
	}
	addTags, err := NormalizeTags(tags)
	if err != nil {
		return nil, err
	}
	if len(addTags) == 0 {
		return nil, fmt.Errorf("at least one tag is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, err := s.loadSessionLocked(sessionID)
	if err != nil {
		return nil, err
	}

	merged, err := NormalizeTags(append(append([]string{}, sess.Tags...), addTags...))
	if err != nil {
		return nil, err
	}
	sess.Tags = merged

	if _, err := os.Stat(s.metaFilePath(sessionID)); os.IsNotExist(err) {
		if err := s.saveFullSessionLocked(sess); err != nil {
			return nil, err
		}
	} else {
		if err := s.writeMetaLocked(sess); err != nil {
			return nil, err
		}
	}
	return sess, nil
}

// RemoveSessionTags removes the specified tags from an existing session.
func (s *Store) RemoveSessionTags(sessionID string, tags []string) (*model.Session, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return nil, err
	}
	rmTags, err := NormalizeTags(tags)
	if err != nil {
		return nil, err
	}
	if len(rmTags) == 0 {
		return nil, fmt.Errorf("at least one tag is required")
	}

	removeSet := make(map[string]struct{}, len(rmTags))
	for _, t := range rmTags {
		removeSet[t] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, err := s.loadSessionLocked(sessionID)
	if err != nil {
		return nil, err
	}

	var remaining []string
	for _, existing := range sess.Tags {
		norm, nErr := NormalizeTag(existing)
		if nErr != nil {
			norm = existing
		}
		if _, drop := removeSet[norm]; !drop {
			remaining = append(remaining, norm)
		}
	}
	sess.Tags = remaining

	if _, err := os.Stat(s.metaFilePath(sessionID)); os.IsNotExist(err) {
		if err := s.saveFullSessionLocked(sess); err != nil {
			return nil, err
		}
	} else {
		if err := s.writeMetaLocked(sess); err != nil {
			return nil, err
		}
	}
	return sess, nil
}

func (s *Store) writeMetaLocked(sess *model.Session) error {
	meta := sessionMeta{
		ID:             sess.ID,
		Command:        sess.Command,
		WorkspaceDir:   sess.WorkspaceDir,
		Tags:           sess.Tags,
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

	framesPath := s.framesFilePath(sessionID)
	repaired, repairedCount, repErr := ensureCleanFramesFile(framesPath)
	if repErr != nil {
		return frame, repErr
	}

	count, ok := s.frameCounts[sessionID]
	if repaired {
		count = repairedCount
	} else if !ok {
		existing, err := readFramesJSONL(framesPath)
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

	f, err := os.OpenFile(framesPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		_ = os.Chmod(framesPath, 0600)
		f, err = os.OpenFile(framesPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return frame, fmt.Errorf("open frames log: %w", err)
		}
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

// ensureCleanFramesFile checks in O(1) whether path ends with '\n'. If a prior crash left a
// partial unterminated line (or a valid final JSON line without '\n'), it atomically rewrites
// the recovered valid frames so subsequent O_APPEND writes never concatenate onto a partial line.
func ensureCleanFramesFile(path string) (bool, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}
	info, sErr := f.Stat()
	if sErr != nil {
		_ = f.Close()
		return false, 0, sErr
	}
	size := info.Size()
	if size == 0 {
		_ = f.Close()
		return false, 0, nil
	}
	var last [1]byte
	_, rErr := f.ReadAt(last[:], size-1)
	_ = f.Close()
	if rErr == nil && last[0] == '\n' {
		return false, 0, nil
	}

	frames, err := readFramesJSONL(path)
	if err != nil {
		return false, 0, err
	}
	var buf bytes.Buffer
	for i := range frames {
		if frames[i].Index <= 0 {
			frames[i].Index = i + 1
		}
		raw, mErr := json.Marshal(frames[i])
		if mErr != nil {
			return false, 0, mErr
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	if err := writeAtomicFile(path, buf.Bytes(), 0600); err != nil {
		return false, 0, err
	}
	return true, len(frames), nil
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
		normTags, _ := NormalizeTags(meta.Tags)
		s.frameCounts[sessionID] = len(frames)
		return &model.Session{
			ID:             meta.ID,
			Command:        meta.Command,
			WorkspaceDir:   meta.WorkspaceDir,
			Tags:           normTags,
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
	if normTags, nErr := NormalizeTags(sess.Tags); nErr == nil {
		sess.Tags = normTags
	}
	if sess.Frames == nil {
		sess.Frames = []model.Frame{}
	}
	sess.FrameCount = len(sess.Frames)
	return &sess, nil
}

// ListSessionIDs returns all candidate session IDs present on disk (both JSONL .meta.json and legacy .json).
func (s *Store) ListSessionIDs() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listSessionIDsLocked()
}

func (s *Store) listSessionIDsLocked() ([]string, error) {
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

	return ids, nil
}

// ListSessions returns all recorded sessions sorted newest StartTime first.
func (s *Store) ListSessions() ([]model.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids, err := s.listSessionIDsLocked()
	if err != nil {
		return nil, err
	}

	sessions := make([]model.Session, 0, len(ids))
	for _, id := range ids {
		if sess, err := s.loadSessionLocked(id); err == nil {
			sessions = append(sessions, *sess)
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].StartTime.Equal(sessions[j].StartTime) {
			return sessions[i].ID > sessions[j].ID
		}
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
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			_ = os.Chmod(p, 0600)
			if retryErr := os.Remove(p); retryErr != nil && !os.IsNotExist(retryErr) && firstErr == nil {
				firstErr = retryErr
			}
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
			_ = os.Chmod(tmpName, 0600)
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
