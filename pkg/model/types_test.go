package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSessionJSONRoundTripAndBackwardCompatibility(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	orig := Session{
		ID:             "trace_100",
		Command:        "go test ./...",
		WorkspaceDir:   "/workspace/repo",
		ExitCode:       0,
		SafetySnapshot: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		StartTime:      now,
		EndTime:        now.Add(2 * time.Second),
		Status:         "completed",
		FrameCount:     2,
		Frames: []Frame{
			{
				Index:       1,
				Timestamp:   now,
				Type:        EventCheckpoint,
				Content:     "Initial workspace snapshot",
				GitSnapshot: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			},
			{
				Index:     2,
				Timestamp: now.Add(time.Second),
				Type:      EventToolCall,
				Content:   "edit file",
				Tool: &ToolInvocation{
					Name:       "edit_file",
					Arguments:  map[string]interface{}{"path": "main.go"},
					Result:     "ok",
					DurationMs: 12,
				},
			},
		},
	}

	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded Session
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if decoded.ID != orig.ID || decoded.WorkspaceDir != orig.WorkspaceDir || len(decoded.Frames) != 2 {
		t.Fatalf("unexpected decoded session: %+v", decoded)
	}
	if decoded.Frames[1].Tool == nil || decoded.Frames[1].Tool.Name != "edit_file" {
		t.Fatalf("tool invocation lost during JSON roundtrip")
	}

	// Verify v1.0.0 legacy session JSON (without workspace_dir / frame_count) unmarshals cleanly.
	legacyJSON := `{"id":"trace_legacy","command":"echo hi","start_time":"2026-11-05T10:00:00Z","status":"completed","frames":[{"index":1,"timestamp":"2026-11-05T10:00:01Z","type":"output","content":"hi"}]}`
	var legacy Session
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatalf("legacy unmarshal failed: %v", err)
	}
	if legacy.ID != "trace_legacy" || len(legacy.Frames) != 1 || legacy.Frames[0].Content != "hi" {
		t.Fatalf("unexpected legacy session: %+v", legacy)
	}
}
