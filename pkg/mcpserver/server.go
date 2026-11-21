// Package mcpserver implements TracePort's native Model Context Protocol (MCP)
// JSON-RPC 2.0 stdio server, exposing redacted time-travel inspection, diff,
// search, and self-healing workspace rewind tools to AI coding agents.
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/redact"
	"github.com/aloneangel45/traceport/pkg/replay"
	"github.com/aloneangel45/traceport/pkg/storage"
)

const (
	maxFrameBytes        = 16 * 1024 * 1024
	defaultListLimit     = 20
	maxListLimit         = 200
	defaultInspectFrames = 50
	maxInspectFrames     = 1000
	defaultSearchLimit   = 20
	maxSearchLimit       = 200
)

// Server serves TracePort flight-recorder and time-travel MCP tools over stdio.
type Server struct {
	store        *storage.Store
	engine       *replay.Engine
	workspaceDir string
	version      string
}

// New constructs a new TracePort MCP server backed by store.
func New(store *storage.Store, workspaceDir, version string) *Server {
	v := strings.TrimSpace(version)
	if v == "" {
		v = "1.3.0"
	}
	return &Server{
		store:        store,
		engine:       replay.NewEngine(store),
		workspaceDir: workspaceDir,
		version:      v,
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type frameMode int

const (
	frameNDJSON frameMode = iota
	frameContentLength
)

// ServeStdio processes JSON-RPC 2.0 MCP messages from r and writes replies to w
// until EOF or ctx cancellation. Auto-detects both NDJSON and Content-Length framing
// for compatibility with Claude Code, Cursor, VS Code, Windsurf, and Antigravity.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	br := bufio.NewReaderSize(r, 64*1024)
	var writeMu sync.Mutex

	writeFrame := func(mode frameMode, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if mode == frameContentLength {
			header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
			if _, err := io.WriteString(w, header); err != nil {
				return err
			}
			_, err := w.Write(payload)
			return err
		}
		if _, err := w.Write(payload); err != nil {
			return err
		}
		_, err := w.Write([]byte("\n"))
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		raw, mode, err := readNextMessage(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}

		respBytes, hasResp := s.handleRawJSON(raw)
		if !hasResp {
			continue
		}
		if err := writeFrame(mode, respBytes); err != nil {
			return err
		}
	}
}

func readNextMessage(br *bufio.Reader) ([]byte, frameMode, error) {
	for {
		b, err := br.Peek(1)
		if err != nil {
			return nil, frameNDJSON, err
		}
		if b[0] == '\r' || b[0] == '\n' || b[0] == ' ' || b[0] == '\t' {
			_, _ = br.ReadByte()
			continue
		}
		break
	}

	peek, _ := br.Peek(16)
	if len(peek) >= 15 && strings.EqualFold(string(peek[:15]), "Content-Length:") {
		contentLen := -1
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return nil, frameContentLength, err
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				break
			}
			if idx := strings.IndexByte(trimmed, ':'); idx > 0 {
				k := strings.TrimSpace(trimmed[:idx])
				v := strings.TrimSpace(trimmed[idx+1:])
				if strings.EqualFold(k, "Content-Length") {
					n, perr := strconv.Atoi(v)
					if perr != nil || n < 0 || n > maxFrameBytes {
						return nil, frameContentLength, fmt.Errorf("invalid Content-Length %q", v)
					}
					contentLen = n
				}
			}
		}
		if contentLen < 0 {
			return nil, frameContentLength, fmt.Errorf("missing Content-Length header")
		}
		buf := make([]byte, contentLen)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, frameContentLength, err
		}
		return buf, frameContentLength, nil
	}

	var lineBuf bytes.Buffer
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 {
			if lineBuf.Len()+len(chunk) > maxFrameBytes {
				return nil, frameNDJSON, fmt.Errorf("JSON-RPC line exceeds %d bytes", maxFrameBytes)
			}
			lineBuf.Write(chunk)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && lineBuf.Len() > 0 {
				return lineBuf.Bytes(), frameNDJSON, nil
			}
			return nil, frameNDJSON, err
		}
		if !isPrefix {
			break
		}
	}
	return lineBuf.Bytes(), frameNDJSON, nil
}

func (s *Server) handleRawJSON(raw []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		resp := rpcResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &rpcError{Code: -32700, Message: "Parse error: empty JSON payload"},
		}
		out, _ := json.Marshal(resp)
		return out, true
	}

	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			resp := rpcResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &rpcError{Code: -32700, Message: "Parse error: " + err.Error()},
			}
			out, _ := json.Marshal(resp)
			return out, true
		}
		if len(batch) == 0 {
			resp := rpcResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &rpcError{Code: -32600, Message: "Invalid Request: empty batch"},
			}
			out, _ := json.Marshal(resp)
			return out, true
		}
		var replies []rpcResponse
		for _, elem := range batch {
			if r, ok := s.handleSingleRequest(elem); ok {
				replies = append(replies, r)
			}
		}
		if len(replies) == 0 {
			return nil, false
		}
		out, _ := json.Marshal(replies)
		return out, true
	}

	resp, ok := s.handleSingleRequest(trimmed)
	if !ok {
		return nil, false
	}
	out, _ := json.Marshal(resp)
	return out, true
}

func (s *Server) handleSingleRequest(raw []byte) (rpcResponse, bool) {
	var req rpcRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &rpcError{Code: -32700, Message: "Parse error: " + err.Error()},
		}, true
	}

	idRaw := bytes.TrimSpace(req.ID)
	// Per JSON-RPC 2.0: Notifications omit "id" entirely and must not receive a response.
	if len(idRaw) == 0 {
		if req.Method == "" {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &rpcError{Code: -32600, Message: "Invalid Request: missing method"},
			}, true
		}
		return rpcResponse{}, false
	}

	if req.Method == "" {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      decodeIDValue(idRaw),
			Error:   &rpcError{Code: -32600, Message: "Invalid Request: missing method"},
		}, true
	}

	return s.dispatch(req, decodeIDValue(idRaw)), true
}

func decodeIDValue(idRaw []byte) any {
	if len(idRaw) == 0 || bytes.Equal(idRaw, []byte("null")) {
		return nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(idRaw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return v
}

func (s *Server) dispatch(req rpcRequest, reqID any) rpcResponse {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		proto := "2024-11-05"
		if p.ProtocolVersion == "2025-03-26" || p.ProtocolVersion == "2024-11-05" {
			proto = p.ProtocolVersion
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Result: map[string]any{
				"protocolVersion": proto,
				"capabilities": map[string]any{
					"tools": map[string]any{
						"listChanged": false,
					},
				},
				"serverInfo": map[string]any{
					"name":    "traceport",
					"version": s.version,
				},
			},
		}

	case "ping":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Result:  map[string]any{},
		}

	case "tools/list":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Result: map[string]any{
				"tools": s.toolDefinitions(),
			},
		}

	case "resources/list":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Result:  map[string]any{"resources": []any{}},
		}

	case "prompts/list":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Result:  map[string]any{"prompts": []any{}},
		}

	case "tools/call":
		var callParams struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      reqID,
				Error:   &rpcError{Code: -32602, Message: "Invalid tools/call params: " + err.Error()},
			}
		}
		text, isErr, rpcErr := s.executeTool(callParams.Name, callParams.Arguments)
		if rpcErr != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      reqID,
				Error:   rpcErr,
			}
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Result: map[string]any{
				"content": []map[string]any{
					{
						"type": "text",
						"text": text,
					},
				},
				"isError": isErr,
			},
		}

	default:
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Error:   &rpcError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)},
		}
	}
}

func (s *Server) toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "list_sessions",
			"description": "List recorded TracePort flight sessions (newest first) with optional tag filtering and secret redaction.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tag": map[string]any{
						"type":        "string",
						"description": "Optional tag (or comma-separated tags) to filter sessions.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of sessions to return (default 20, max 200).",
					},
				},
			},
		},
		{
			"name":        "inspect_session",
			"description": "Inspect a recorded TracePort session's metadata and redacted chronological frame timeline.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{
						"type":        "string",
						"description": "Recorded TracePort session ID (e.g. trace_1794348120984102).",
					},
					"max_frames": map[string]any{
						"type":        "integer",
						"description": "Maximum number of frames to return (default 50, max 1000).",
					},
				},
				"required": []string{"session_id"},
			},
		},
		{
			"name":        "diff_checkpoints",
			"description": "Compute a redacted unified filesystem diff between two steps of a session, or between from_step and the current live workspace when to_step is 0.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{
						"type":        "string",
						"description": "Recorded TracePort session ID.",
					},
					"from_step": map[string]any{
						"type":        "integer",
						"description": "Source step index (1-based, default 1).",
					},
					"to_step": map[string]any{
						"type":        "integer",
						"description": "Target step index (0 compares against current live workspace).",
					},
				},
				"required": []string{"session_id"},
			},
		},
		{
			"name":        "search_traces",
			"description": "Search across recorded TracePort sessions (commands, tags, stdout/stderr frames, tool calls, and checkpoint file changes) with automatic credential redaction.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Substring or regular expression to search for.",
					},
					"regex": map[string]any{
						"type":        "boolean",
						"description": "Treat query as a regular expression when true.",
					},
					"tag": map[string]any{
						"type":        "string",
						"description": "Optional tag (or comma-separated tags) to restrict search scope.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of matching sessions to return (default 20, max 200).",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "rewind_workspace",
			"description": "Roll back workspace files and session timeline to a prior step. Always creates a mandatory pre-rewind safety savepoint unless dry_run=true.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{
						"type":        "string",
						"description": "Recorded TracePort session ID.",
					},
					"step": map[string]any{
						"type":        "integer",
						"description": "1-based target step index to rewind to.",
					},
					"dry_run": map[string]any{
						"type":        "boolean",
						"description": "When true, preview file mutations without modifying the workspace.",
					},
				},
				"required": []string{"session_id", "step"},
			},
		},
	}
}

func (s *Server) executeTool(name string, rawArgs json.RawMessage) (string, bool, *rpcError) {
	if s.store == nil {
		return "traceport store is not initialized", true, nil
	}
	switch name {
	case "list_sessions":
		return s.toolListSessions(rawArgs)
	case "inspect_session":
		return s.toolInspectSession(rawArgs)
	case "diff_checkpoints":
		return s.toolDiffCheckpoints(rawArgs)
	case "search_traces":
		return s.toolSearchTraces(rawArgs)
	case "rewind_workspace":
		return s.toolRewindWorkspace(rawArgs)
	default:
		return "", false, &rpcError{Code: -32602, Message: fmt.Sprintf("Unknown tool: %s", name)}
	}
}

func unmarshalArgs(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return json.Unmarshal(trimmed, dst)
}

func marshalToolJSON(v any) (string, bool, *rpcError) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("failed to encode tool response: %v", err), true, nil
	}
	return string(data), false, nil
}

func (s *Server) toolListSessions(rawArgs json.RawMessage) (string, bool, *rpcError) {
	var args struct {
		Tag   string `json:"tag"`
		Limit int    `json:"limit"`
	}
	if err := unmarshalArgs(rawArgs, &args); err != nil {
		return "", false, &rpcError{Code: -32602, Message: "invalid arguments for list_sessions: " + err.Error()}
	}

	limit := args.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	var filterTags []string
	if strings.TrimSpace(args.Tag) != "" {
		var err error
		filterTags, err = storage.NormalizeTags([]string{args.Tag})
		if err != nil {
			return fmt.Sprintf("invalid tag filter: %v", err), true, nil
		}
	}

	all, err := s.store.ListSessions()
	if err != nil {
		return fmt.Sprintf("failed listing sessions: %v", err), true, nil
	}

	type sessionSummary struct {
		ID             string   `json:"id"`
		Command        string   `json:"command"`
		WorkspaceDir   string   `json:"workspace_dir,omitempty"`
		Tags           []string `json:"tags,omitempty"`
		Status         string   `json:"status"`
		ExitCode       int      `json:"exit_code"`
		StartTime      string   `json:"start_time"`
		EndTime        string   `json:"end_time,omitempty"`
		FrameCount     int      `json:"frame_count"`
		SafetySnapshot string   `json:"safety_snapshot,omitempty"`
	}

	matched := make([]sessionSummary, 0)
	for _, sess := range all {
		if len(filterTags) > 0 && !storage.MatchesAllTags(sess, filterTags) {
			continue
		}
		item := sessionSummary{
			ID:             sess.ID,
			Command:        redact.String(sess.Command),
			WorkspaceDir:   redact.String(sess.WorkspaceDir),
			Tags:           sess.Tags,
			Status:         sess.Status,
			ExitCode:       sess.ExitCode,
			StartTime:      sess.StartTime.UTC().Format("2006-01-02T15:04:05Z07:00"),
			FrameCount:     len(sess.Frames),
			SafetySnapshot: sess.SafetySnapshot,
		}
		if !sess.EndTime.IsZero() {
			item.EndTime = sess.EndTime.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		matched = append(matched, item)
	}

	total := len(matched)
	if len(matched) > limit {
		matched = matched[:limit]
	}

	return marshalToolJSON(map[string]any{
		"total":    total,
		"returned": len(matched),
		"sessions": matched,
	})
}

func (s *Server) toolInspectSession(rawArgs json.RawMessage) (string, bool, *rpcError) {
	var args struct {
		SessionID string `json:"session_id"`
		MaxFrames int    `json:"max_frames"`
	}
	if err := unmarshalArgs(rawArgs, &args); err != nil {
		return "", false, &rpcError{Code: -32602, Message: "invalid arguments for inspect_session: " + err.Error()}
	}
	if strings.TrimSpace(args.SessionID) == "" {
		return "", false, &rpcError{Code: -32602, Message: "session_id is required"}
	}

	maxFrames := args.MaxFrames
	if maxFrames <= 0 {
		maxFrames = defaultInspectFrames
	}
	if maxFrames > maxInspectFrames {
		maxFrames = maxInspectFrames
	}

	sess, err := s.store.GetSession(args.SessionID)
	if err != nil {
		return redact.String(err.Error()), true, nil
	}

	clean := redact.Session(sess)
	totalFrames := len(clean.Frames)
	truncated := false
	frames := clean.Frames
	if len(frames) > maxFrames {
		frames = frames[:maxFrames]
		truncated = true
	}
	if frames == nil {
		frames = []model.Frame{}
	}

	out := map[string]any{
		"session_id":      clean.ID,
		"command":         clean.Command,
		"workspace_dir":   clean.WorkspaceDir,
		"tags":            clean.Tags,
		"status":          clean.Status,
		"exit_code":       clean.ExitCode,
		"start_time":      clean.StartTime,
		"safety_snapshot": clean.SafetySnapshot,
		"total_frames":    totalFrames,
		"returned_frames": len(frames),
		"truncated":       truncated,
		"redacted":        true,
		"frames":          frames,
	}
	if !clean.EndTime.IsZero() {
		out["end_time"] = clean.EndTime
	}
	return marshalToolJSON(out)
}

func (s *Server) toolDiffCheckpoints(rawArgs json.RawMessage) (string, bool, *rpcError) {
	var args struct {
		SessionID string `json:"session_id"`
		FromStep  int    `json:"from_step"`
		ToStep    int    `json:"to_step"`
	}
	if err := unmarshalArgs(rawArgs, &args); err != nil {
		return "", false, &rpcError{Code: -32602, Message: "invalid arguments for diff_checkpoints: " + err.Error()}
	}
	if strings.TrimSpace(args.SessionID) == "" {
		return "", false, &rpcError{Code: -32602, Message: "session_id is required"}
	}
	fromStep := args.FromStep
	if fromStep == 0 {
		fromStep = 1
	}

	workDir := s.resolveWorkspaceDir()
	report, err := s.engine.DiffSession(args.SessionID, fromStep, args.ToStep, workDir)
	if err != nil {
		return redact.String(err.Error()), true, nil
	}

	// Scrub all diff content and file paths so .env secrets / API keys never leak into LLM context.
	redactedReport := &replay.DiffReport{
		SessionID:   report.SessionID,
		FromStep:    report.FromStep,
		ToStep:      report.ToStep,
		FromTree:    report.FromTree,
		ToTree:      report.ToTree,
		FilesAdded:  report.FilesAdded,
		FilesMod:    report.FilesMod,
		FilesDel:    report.FilesDel,
		Changes:     make([]replay.FileDiffEntry, len(report.Changes)),
		UnifiedText: redact.String(report.UnifiedText),
	}
	for i, ch := range report.Changes {
		redactedReport.Changes[i] = replay.FileDiffEntry{
			Path:    redact.String(ch.Path),
			Status:  ch.Status,
			Unified: redact.String(ch.Unified),
		}
	}

	return marshalToolJSON(redactedReport)
}

func (s *Server) toolSearchTraces(rawArgs json.RawMessage) (string, bool, *rpcError) {
	var args struct {
		Query string `json:"query"`
		Regex bool   `json:"regex"`
		Tag   string `json:"tag"`
		Limit int    `json:"limit"`
	}
	if err := unmarshalArgs(rawArgs, &args); err != nil {
		return "", false, &rpcError{Code: -32602, Message: "invalid arguments for search_traces: " + err.Error()}
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", false, &rpcError{Code: -32602, Message: "query is required"}
	}

	limit := args.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	var tags []string
	if strings.TrimSpace(args.Tag) != "" {
		tags = []string{args.Tag}
	}

	rawResults, err := s.engine.Search(replay.SearchOptions{
		Query: args.Query,
		Regex: args.Regex,
		Tags:  tags,
	})
	if err != nil {
		return redact.String(err.Error()), true, nil
	}

	totalSessions := len(rawResults)
	if len(rawResults) > limit {
		rawResults = rawResults[:limit]
	}

	// Redact every command, snippet, and file path before returning to LLM context.
	cleanResults := make([]replay.SessionSearchResult, len(rawResults))
	for i, r := range rawResults {
		matches := make([]replay.SearchHit, len(r.Matches))
		for j, m := range r.Matches {
			matches[j] = replay.SearchHit{
				Source:   m.Source,
				Step:     m.Step,
				Type:     m.Type,
				FilePath: redact.String(m.FilePath),
				Snippet:  redact.String(m.Snippet),
			}
		}
		cleanResults[i] = replay.SessionSearchResult{
			SessionID: r.SessionID,
			Command:   redact.String(r.Command),
			Status:    r.Status,
			Tags:      r.Tags,
			StartTime: r.StartTime,
			Matches:   matches,
		}
	}

	return marshalToolJSON(map[string]any{
		"query":             redact.String(args.Query),
		"total_sessions":    totalSessions,
		"returned_sessions": len(cleanResults),
		"redacted":          true,
		"results":           cleanResults,
	})
}

func (s *Server) toolRewindWorkspace(rawArgs json.RawMessage) (string, bool, *rpcError) {
	var args struct {
		SessionID string `json:"session_id"`
		Step      int    `json:"step"`
		DryRun    bool   `json:"dry_run"`
	}
	if err := unmarshalArgs(rawArgs, &args); err != nil {
		return "", false, &rpcError{Code: -32602, Message: "invalid arguments for rewind_workspace: " + err.Error()}
	}
	if strings.TrimSpace(args.SessionID) == "" {
		return "", false, &rpcError{Code: -32602, Message: "session_id is required"}
	}
	if args.Step <= 0 {
		return "", false, &rpcError{Code: -32602, Message: "step must be >= 1"}
	}

	workDir := s.resolveWorkspaceDir()

	// For live rewinds (dry_run=false), enforce that a pre-rewind safety savepoint can always be captured.
	var fallbackSafety string
	if !args.DryRun {
		sess, err := s.store.GetSession(args.SessionID)
		if err != nil {
			return redact.String(err.Error()), true, nil
		}
		if args.Step > len(sess.Frames) {
			return fmt.Sprintf("target step %d out of bounds (1..%d)", args.Step, len(sess.Frames)), true, nil
		}
		hasSnapshot := false
		for i := args.Step - 1; i >= 0; i-- {
			if sess.Frames[i].GitSnapshot != "" {
				hasSnapshot = true
				break
			}
		}
		// Pre-verify / capture safety savepoint if the session had no prior checkpoint hash,
		// so EVERY non-dry-run rewind guarantees a mandatory safety savepoint.
		if !hasSnapshot && workDir != "" {
			var treeOpts []checkpoint.TreeOption
			if strings.HasPrefix(sess.Command, "traceport watch") {
				treeOpts = append(treeOpts, checkpoint.WithIgnoreFiltering())
			}
			ts, tErr := checkpoint.NewTreeStore(workDir, s.store.RootDir(), treeOpts...)
			if tErr != nil {
				return fmt.Sprintf("mandatory pre-rewind safety savepoint failed: %v", tErr), true, nil
			}
			snap, sErr := ts.CreateSnapshot()
			_ = ts.Close()
			if sErr != nil || snap == "" {
				return fmt.Sprintf("mandatory pre-rewind safety savepoint failed: %v", sErr), true, nil
			}
			fallbackSafety = snap
		}
	}

	res, err := s.engine.RewindWithOptions(args.SessionID, args.Step, workDir, replay.RewindOptions{
		Force:  false,
		DryRun: args.DryRun,
	})
	if err != nil {
		return redact.String(err.Error()), true, nil
	}

	if !args.DryRun && res.SafetySnapshot == "" && fallbackSafety != "" {
		res.SafetySnapshot = fallbackSafety
		if res.Session != nil {
			res.Session.SafetySnapshot = fallbackSafety
			_ = s.store.UpdateSessionMeta(res.Session)
		}
	}

	resp := map[string]any{
		"session_id":      args.SessionID,
		"target_step":     res.TargetStep,
		"dry_run":         res.DryRun,
		"snapshot_hash":   res.SnapshotHash,
		"safety_snapshot": res.SafetySnapshot,
	}
	if res.Session != nil {
		resp["status"] = res.Session.Status
		resp["command"] = redact.String(res.Session.Command)
		resp["workspace_dir"] = redact.String(res.Session.WorkspaceDir)
		resp["remaining_frames"] = len(res.Session.Frames)
	}
	if res.Plan != nil {
		resp["plan"] = res.Plan
	}
	if res.RestoreStats != nil {
		resp["restore_stats"] = res.RestoreStats
	}

	return marshalToolJSON(resp)
}

func (s *Server) resolveWorkspaceDir() string {
	if strings.TrimSpace(s.workspaceDir) != "" {
		return s.workspaceDir
	}
	cwd, _ := os.Getwd()
	return cwd
}
