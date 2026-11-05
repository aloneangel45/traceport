package storage

import (
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
		ID:        "test_session_1",
		Command:   "go version",
		StartTime: time.Now(),
		Status:    "running",
		Frames:    []model.Frame{},
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("failed to save session: %v", err)
	}

	frame := model.Frame{
		Timestamp: time.Now(),
		Type:      model.EventOutput,
		Content:   "go version go1.22.0 windows/amd64",
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
