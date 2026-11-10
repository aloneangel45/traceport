package model

import "time"

// EventType defines the type of frame recorded in the flight session.
type EventType string

const (
	EventInput      EventType = "input"       // User prompt or stdin input
	EventOutput     EventType = "output"      // Agent output / stdout / thought
	EventToolCall   EventType = "tool_call"   // Tool or function call invocation
	EventToolResult EventType = "tool_result" // Output/response from a tool
	EventError      EventType = "error"       // Execution error or stderr
	EventCheckpoint EventType = "checkpoint"  // Filesystem / shadow-tree workspace savepoint
)

// ToolInvocation details a tool call made by an agent.
type ToolInvocation struct {
	Name       string                 `json:"name"`
	Arguments  map[string]interface{} `json:"arguments"`
	Result     string                 `json:"result,omitempty"`
	DurationMs int64                  `json:"duration_ms,omitempty"`
}

// Frame represents a single deterministic flight frame.
type Frame struct {
	Index       int             `json:"index"`
	Timestamp   time.Time       `json:"timestamp"`
	Type        EventType       `json:"type"`
	Content     string          `json:"content"`
	Tool        *ToolInvocation `json:"tool,omitempty"`
	GitSnapshot string          `json:"git_snapshot,omitempty"`
}

// Session represents an entire agent execution flight.
type Session struct {
	ID             string    `json:"id"`
	Command        string    `json:"command"`
	WorkspaceDir   string    `json:"workspace_dir,omitempty"`
	ExitCode       int       `json:"exit_code,omitempty"`
	SafetySnapshot string    `json:"safety_snapshot,omitempty"`
	StartTime      time.Time `json:"start_time"`
	EndTime        time.Time `json:"end_time,omitempty"`
	Status         string    `json:"status"` // running, completed, aborted, rewound_to_step_N
	FrameCount     int       `json:"frame_count,omitempty"`
	Frames         []Frame   `json:"frames"`
}
