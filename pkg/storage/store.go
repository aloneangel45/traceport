package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/aloneangel45/traceport/pkg/model"
)

type Store struct {
	baseDir string
	mu      sync.Mutex
}

func NewStore() (*Store, error) {
	appData := os.Getenv("APPDATA")
	var dir string
	if appData != "" {
		dir = filepath.Join(appData, "TracePort", "sessions")
	} else {
		dir = filepath.Join(".traceport", "sessions")
	}

	return NewStoreWithPath(dir)
}

func NewStoreWithPath(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &Store{baseDir: dir}, nil
}

func (s *Store) sessionFilePath(id string) string {
	return filepath.Join(s.baseDir, fmt.Sprintf("%s.json", id))
}

func (s *Store) SaveSession(sess *model.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.sessionFilePath(sess.ID), data, 0644)
}

func (s *Store) AppendFrame(sessionID string, frame model.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, err := s.loadSessionUnlocked(sessionID)
	if err != nil {
		return err
	}

	frame.Index = len(sess.Frames) + 1
	sess.Frames = append(sess.Frames, frame)

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.sessionFilePath(sessionID), data, 0644)
}

func (s *Store) GetSession(sessionID string) (*model.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.loadSessionUnlocked(sessionID)
}

func (s *Store) loadSessionUnlocked(sessionID string) (*model.Session, error) {
	path := s.sessionFilePath(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session %s not found: %w", sessionID, err)
	}

	var sess model.Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("corrupt session %s: %w", sessionID, err)
	}

	return &sess, nil
}

func (s *Store) ListSessions() ([]model.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, err
	}

	var sessions []model.Session
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			data, err := os.ReadFile(filepath.Join(s.baseDir, entry.Name()))
			if err == nil {
				var sess model.Session
				if err := json.Unmarshal(data, &sess); err == nil {
					sessions = append(sessions, sess)
				}
			}
		}
	}

	// Sort newest first
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartTime.After(sessions[j].StartTime)
	})

	return sessions, nil
}
