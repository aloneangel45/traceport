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
	"unicode/utf8"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/redact"
	"github.com/aloneangel45/traceport/pkg/replay"
	"github.com/aloneangel45/traceport/pkg/storage"
)

const (
	maxFrameBytes           = 16 * 1024 * 1024
	maxHeaderLineBytes      = 16 * 1024
	maxHeaderLines          = 64
	maxBatchSize            = 100
	defaultListLimit        = 20
	maxListLimit            = 200
	defaultInspectFrames    = 50
	maxInspectFrames        = 1000
	defaultSearchLimit      = 20
	maxSearchLimit          = 200
	maxSearchMatchesPerSess = 50
	maxFrameContentRunes    = 16 * 1024
	maxToolFieldRunes       = 8 * 1024
	maxInspectTotalRunes    = 192 * 1024
	maxDiffFileRunes        = 24 * 1024
	maxDiffTotalRunes       = 128 * 1024
	maxDiffFiles            = 100
	maxPlanPaths            = 200
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
		v = "1.3.1"
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

func readBoundedLine(br *bufio.Reader, maxBytes int) ([]byte, error) {
	var lineBuf bytes.Buffer
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 {
			if lineBuf.Len()+len(chunk) > maxBytes {
				return nil, fmt.Errorf("JSON-RPC line exceeds %d bytes", maxBytes)
			}
			lineBuf.Write(chunk)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && lineBuf.Len() > 0 {
				return lineBuf.Bytes(), nil
			}
			return nil, err
		}
		if !isPrefix {
			break
		}
	}
	return lineBuf.Bytes(), nil
}

func parseMIMEHeaderLine(line []byte) (key, val string, ok bool) {
	trimmed := strings.TrimSpace(string(line))
	idx := strings.IndexByte(trimmed, ':')
	if idx <= 0 {
		return "", "", false
	}
	k := strings.TrimSpace(trimmed[:idx])
	if k == "" {
		return "", "", false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
			return "", "", false
		}
	}
	return k, strings.TrimSpace(trimmed[idx+1:]), true
}

func isFramingHeaderKey(k string) bool {
	lower := strings.ToLower(k)
	return lower == "content-length" ||
		lower == "content-type" ||
		strings.HasPrefix(lower, "content-") ||
		strings.HasPrefix(lower, "x-") ||
		strings.HasPrefix(lower, "mime-")
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

	firstByte, err := br.Peek(1)
	if err != nil {
		return nil, frameNDJSON, err
	}
	if firstByte[0] == '{' || firstByte[0] == '[' {
		line, err := readBoundedLine(br, maxFrameBytes)
		if err != nil {
			return nil, frameNDJSON, err
		}
		return line, frameNDJSON, nil
	}

	firstLine, err := readBoundedLine(br, maxFrameBytes)
	if err != nil {
		return nil, frameNDJSON, err
	}
	k, v, isHdr := parseMIMEHeaderLine(firstLine)
	if !isHdr || !isFramingHeaderKey(k) {
		return firstLine, frameNDJSON, nil
	}
	if len(firstLine) > maxHeaderLineBytes {
		return nil, frameContentLength, fmt.Errorf("MIME header line exceeds %d bytes", maxHeaderLineBytes)
	}

	contentLen := -1
	seenContentLen := false
	processHeader := func(hk, hv string) error {
		if !strings.EqualFold(hk, "Content-Length") {
			return nil
		}
		n, perr := strconv.Atoi(hv)
		if perr != nil || n < 0 || n > maxFrameBytes {
			return fmt.Errorf("invalid Content-Length %q", redact.String(hv))
		}
		if seenContentLen && contentLen != n {
			return fmt.Errorf("conflicting duplicate Content-Length headers")
		}
		contentLen = n
		seenContentLen = true
		return nil
	}

	if err := processHeader(k, v); err != nil {
		return nil, frameContentLength, err
	}

	headerCount := 1
	for {
		line, err := readBoundedLine(br, maxHeaderLineBytes)
		if err != nil {
			return nil, frameContentLength, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			break
		}
		headerCount++
		if headerCount > maxHeaderLines {
			return nil, frameContentLength, fmt.Errorf("too many MIME headers (max %d)", maxHeaderLines)
		}
		hk, hv, ok := parseMIMEHeaderLine(line)
		if !ok {
			return nil, frameContentLength, fmt.Errorf("malformed MIME header line")
		}
		if err := processHeader(hk, hv); err != nil {
			return nil, frameContentLength, err
		}
	}

	if !seenContentLen || contentLen < 0 {
		return nil, frameContentLength, fmt.Errorf("missing Content-Length header")
	}
	buf := make([]byte, contentLen)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, frameContentLength, err
	}
	return buf, frameContentLength, nil
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
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		var batch []json.RawMessage
		if err := dec.Decode(&batch); err != nil {
			resp := rpcResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &rpcError{Code: -32700, Message: redact.String("Parse error: " + err.Error())},
			}
			out, _ := json.Marshal(resp)
			return out, true
		}
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			resp := rpcResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &rpcError{Code: -32700, Message: "Parse error: unexpected trailing data after JSON array"},
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
		if len(batch) > maxBatchSize {
			resp := rpcResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &rpcError{Code: -32600, Message: fmt.Sprintf("Invalid Request: batch size %d exceeds maximum %d", len(batch), maxBatchSize)},
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
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &rpcError{Code: -32700, Message: "Parse error: empty JSON payload"},
		}, true
	}

	// Validate syntactically complete JSON with zero trailing garbage before type checking.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var probe any
	if err := dec.Decode(&probe); err != nil {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &rpcError{Code: -32700, Message: redact.String("Parse error: " + err.Error())},
		}, true
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &rpcError{Code: -32700, Message: "Parse error: unexpected trailing data after JSON payload"},
		}, true
	}

	// Valid JSON that is not an object (e.g. number, string, bool, null, nested array) is Invalid Request (-32600).
	if trimmed[0] != '{' {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &rpcError{Code: -32600, Message: "Invalid Request: request must be a JSON object"},
		}, true
	}

	// Decode into raw field map so we accurately distinguish omitted "id" (notification)
	// from explicit `"id": null` (request requiring a response with `"id": null`).
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &rpcError{Code: -32700, Message: redact.String("Parse error: " + err.Error())},
		}, true
	}

	idRaw, hasID := fields["id"]
	var reqID any
	if hasID {
		var idOK bool
		reqID, idOK = decodeIDValue(bytes.TrimSpace(idRaw))
		if !idOK {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &rpcError{Code: -32600, Message: "Invalid Request: id must be a string, number, or null"},
			}, true
		}
	}

	var version string
	if verRaw, ok := fields["jsonrpc"]; !ok || json.Unmarshal(verRaw, &version) != nil || version != "2.0" {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Error:   &rpcError{Code: -32600, Message: `Invalid Request: "jsonrpc" must be "2.0"`},
		}, true
	}

	var method string
	if methodRaw, ok := fields["method"]; !ok || json.Unmarshal(methodRaw, &method) != nil || method == "" {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Error:   &rpcError{Code: -32600, Message: "Invalid Request: missing or invalid method"},
		}, true
	}

	var params json.RawMessage
	if paramsRaw, ok := fields["params"]; ok {
		pt := bytes.TrimSpace(paramsRaw)
		if len(pt) > 0 && !bytes.Equal(pt, []byte("null")) && pt[0] != '{' && pt[0] != '[' {
			if !hasID {
				return rpcResponse{}, false
			}
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      reqID,
				Error:   &rpcError{Code: -32602, Message: "Invalid params: params must be an object or array"},
			}, true
		}
		params = pt
	}

	// Per JSON-RPC 2.0: Notifications omit "id" entirely and must not receive a response.
	if !hasID {
		return rpcResponse{}, false
	}

	req := rpcRequest{
		JSONRPC: version,
		ID:      idRaw,
		Method:  method,
		Params:  params,
	}
	return s.dispatch(req, reqID), true
}

func decodeIDValue(idRaw []byte) (any, bool) {
	if len(idRaw) == 0 || bytes.Equal(idRaw, []byte("null")) {
		return nil, true
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(idRaw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	switch v.(type) {
	case string, json.Number:
		return v, true
	default:
		return nil, false
	}
}

func (s *Server) dispatch(req rpcRequest, reqID any) rpcResponse {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 && !bytes.Equal(req.Params, []byte("null")) {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return rpcResponse{
					JSONRPC: "2.0",
					ID:      reqID,
					Error:   &rpcError{Code: -32602, Message: redact.String("Invalid initialize params: " + err.Error())},
				}
			}
		}
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
		if len(req.Params) == 0 || bytes.Equal(req.Params, []byte("null")) || req.Params[0] != '{' {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      reqID,
				Error:   &rpcError{Code: -32602, Message: "Invalid tools/call params: expected JSON object"},
			}
		}
		var callParams struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      reqID,
				Error:   &rpcError{Code: -32602, Message: redact.String("Invalid tools/call params: " + err.Error())},
			}
		}
		if strings.TrimSpace(callParams.Name) == "" {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      reqID,
				Error:   &rpcError{Code: -32602, Message: "Invalid tools/call params: tool name is required"},
			}
		}
		text, isErr, rpcErr := s.executeTool(callParams.Name, callParams.Arguments)
		if rpcErr != nil {
			rpcErr.Message = redact.String(rpcErr.Message)
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      reqID,
				Error:   rpcErr,
			}
		}
		if isErr {
			text = redact.String(text)
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
			Error:   &rpcError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", redact.String(req.Method))},
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
		return "", false, &rpcError{Code: -32602, Message: fmt.Sprintf("Unknown tool: %s", redact.String(name))}
	}
}

func unmarshalArgs(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] != '{' {
		return fmt.Errorf("arguments must be a JSON object")
	}
	return json.Unmarshal(trimmed, dst)
}

func marshalToolJSON(v any) (string, bool, *rpcError) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return redact.String(fmt.Sprintf("failed to encode tool response: %v", err)), true, nil
	}
	return string(data), false, nil
}

func redactStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = redact.String(s)
	}
	return out
}

func truncateMCPRunes(s string, maxRunes int, suffix string) (string, bool) {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s, false
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + suffix, true
}

func capMCPTextWithBudget(s string, maxItemRunes int, remainingBudget *int) (string, bool) {
	origRunes := utf8.RuneCountInString(s)
	limit := maxItemRunes
	if remainingBudget != nil {
		if *remainingBudget <= 0 {
			return fmt.Sprintf("... [truncated: MCP response size cap reached; %d bytes omitted]", len(s)), true
		}
		if *remainingBudget < limit {
			limit = *remainingBudget
		}
	}
	if origRunes <= limit {
		if remainingBudget != nil {
			*remainingBudget -= origRunes
		}
		return s, false
	}
	out, _ := truncateMCPRunes(s, limit, fmt.Sprintf("\n... [truncated: %d total bytes]", len(s)))
	if remainingBudget != nil {
		*remainingBudget -= limit
		if *remainingBudget < 0 {
			*remainingBudget = 0
		}
	}
	return out, true
}

func (s *Server) toolListSessions(rawArgs json.RawMessage) (string, bool, *rpcError) {
	var args struct {
		Tag   string `json:"tag"`
		Limit int    `json:"limit"`
	}
	if err := unmarshalArgs(rawArgs, &args); err != nil {
		return "", false, &rpcError{Code: -32602, Message: redact.String("invalid arguments for list_sessions: " + err.Error())}
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
			return redact.String(fmt.Sprintf("invalid tag filter: %v", err)), true, nil
		}
	}

	all, err := s.store.ListSessions()
	if err != nil {
		return redact.String(fmt.Sprintf("failed listing sessions: %v", err)), true, nil
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
			ID:             redact.String(sess.ID),
			Command:        redact.String(sess.Command),
			WorkspaceDir:   redact.String(sess.WorkspaceDir),
			Tags:           redactStringSlice(sess.Tags),
			Status:         redact.String(sess.Status),
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
		return "", false, &rpcError{Code: -32602, Message: redact.String("invalid arguments for inspect_session: " + err.Error())}
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
	rawFrames := clean.Frames
	if len(rawFrames) > maxFrames {
		rawFrames = rawFrames[:maxFrames]
		truncated = true
	}

	remainingBudget := maxInspectTotalRunes
	frames := make([]model.Frame, len(rawFrames))
	for i, f := range rawFrames {
		fc := f
		if fc.Content != "" {
			var wasCut bool
			fc.Content, wasCut = capMCPTextWithBudget(fc.Content, maxFrameContentRunes, &remainingBudget)
			if wasCut {
				truncated = true
			}
		}
		if f.Tool != nil {
			tc := *f.Tool
			if tc.Result != "" {
				var wasCut bool
				tc.Result, wasCut = capMCPTextWithBudget(tc.Result, maxToolFieldRunes, &remainingBudget)
				if wasCut {
					truncated = true
				}
			}
			if len(tc.Arguments) > 0 {
				if rawArgBytes, mErr := json.Marshal(tc.Arguments); mErr == nil && utf8.RuneCount(rawArgBytes) > maxToolFieldRunes {
					preview, _ := capMCPTextWithBudget(redact.String(string(rawArgBytes)), maxToolFieldRunes, &remainingBudget)
					tc.Arguments = map[string]any{
						"_truncated": true,
						"_preview":   preview,
					}
					truncated = true
				}
			}
			fc.Tool = &tc
		}
		frames[i] = fc
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
		return "", false, &rpcError{Code: -32602, Message: redact.String("invalid arguments for diff_checkpoints: " + err.Error())}
	}
	if strings.TrimSpace(args.SessionID) == "" {
		return "", false, &rpcError{Code: -32602, Message: "session_id is required"}
	}
	if args.FromStep < 0 {
		return "", false, &rpcError{Code: -32602, Message: "from_step must be >= 0"}
	}
	if args.ToStep < 0 {
		return "", false, &rpcError{Code: -32602, Message: "to_step must be >= 0"}
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

	// Scrub all diff content and file paths BEFORE truncating so partial secrets
	// at truncation boundaries can never leak into LLM context.
	truncated := false
	omittedFiles := 0
	remainingBudget := maxDiffTotalRunes

	maxChanges := len(report.Changes)
	if maxChanges > maxDiffFiles {
		omittedFiles = maxChanges - maxDiffFiles
		maxChanges = maxDiffFiles
		truncated = true
	}

	changes := make([]replay.FileDiffEntry, 0, maxChanges)
	var unifiedBuilder strings.Builder
	for i := 0; i < maxChanges; i++ {
		ch := report.Changes[i]
		safePath := redact.String(ch.Path)
		scrubbedDiff := redact.String(ch.Unified)
		cappedDiff, wasCut := capMCPTextWithBudget(scrubbedDiff, maxDiffFileRunes, &remainingBudget)
		if wasCut {
			truncated = true
		}
		changes = append(changes, replay.FileDiffEntry{
			Path:    safePath,
			Status:  ch.Status,
			Unified: cappedDiff,
		})
		unifiedBuilder.WriteString(cappedDiff)
		if cappedDiff != "" && !strings.HasSuffix(cappedDiff, "\n") {
			unifiedBuilder.WriteByte('\n')
		}
	}

	return marshalToolJSON(map[string]any{
		"session_id":     redact.String(report.SessionID),
		"from_step":      report.FromStep,
		"to_step":        report.ToStep,
		"from_tree":      report.FromTree,
		"to_tree":        report.ToTree,
		"files_added":    report.FilesAdded,
		"files_modified": report.FilesMod,
		"files_deleted":  report.FilesDel,
		"omitted_files":  omittedFiles,
		"truncated":      truncated,
		"redacted":       true,
		"changes":        changes,
		"unified_text":   unifiedBuilder.String(),
	})
}

func (s *Server) toolSearchTraces(rawArgs json.RawMessage) (string, bool, *rpcError) {
	var args struct {
		Query string `json:"query"`
		Regex bool   `json:"regex"`
		Tag   string `json:"tag"`
		Limit int    `json:"limit"`
	}
	if err := unmarshalArgs(rawArgs, &args); err != nil {
		return "", false, &rpcError{Code: -32602, Message: redact.String("invalid arguments for search_traces: " + err.Error())}
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

	// Pass Redact: true so credentials are scrubbed BEFORE matching, blocking secret-extraction oracles.
	rawResults, err := s.engine.Search(replay.SearchOptions{
		Query:  args.Query,
		Regex:  args.Regex,
		Tags:   tags,
		Redact: true,
	})
	if err != nil {
		return redact.String(err.Error()), true, nil
	}

	totalSessions := len(rawResults)
	truncated := false
	if len(rawResults) > limit {
		rawResults = rawResults[:limit]
		truncated = true
	}

	// Redact and cap every command, tag, snippet, and file path before returning to LLM context.
	cleanResults := make([]replay.SessionSearchResult, len(rawResults))
	for i, r := range rawResults {
		rawMatches := r.Matches
		if len(rawMatches) > maxSearchMatchesPerSess {
			rawMatches = rawMatches[:maxSearchMatchesPerSess]
			truncated = true
		}
		matches := make([]replay.SearchHit, len(rawMatches))
		for j, m := range rawMatches {
			matches[j] = replay.SearchHit{
				Source:   m.Source,
				Step:     m.Step,
				Type:     m.Type,
				FilePath: redact.String(m.FilePath),
				Snippet:  redact.String(m.Snippet),
			}
		}
		cleanResults[i] = replay.SessionSearchResult{
			SessionID: redact.String(r.SessionID),
			Command:   redact.String(r.Command),
			Status:    redact.String(r.Status),
			Tags:      redactStringSlice(r.Tags),
			StartTime: r.StartTime,
			Matches:   matches,
		}
	}

	return marshalToolJSON(map[string]any{
		"query":             redact.String(args.Query),
		"total_sessions":    totalSessions,
		"returned_sessions": len(cleanResults),
		"truncated":         truncated,
		"redacted":          true,
		"results":           cleanResults,
	})
}

func redactAndCapPlan(plan *checkpoint.RestorePlan) (*checkpoint.RestorePlan, bool) {
	if plan == nil {
		return nil, false
	}
	truncated := false
	capList := func(in []string) []string {
		if in == nil {
			return []string{}
		}
		n := len(in)
		if n > maxPlanPaths {
			n = maxPlanPaths
			truncated = true
		}
		out := make([]string, n)
		for i := 0; i < n; i++ {
			out[i] = redact.String(in[i])
		}
		return out
	}
	return &checkpoint.RestorePlan{
		TreeHash:  plan.TreeHash,
		Create:    capList(plan.Create),
		Modify:    capList(plan.Modify),
		Delete:    capList(plan.Delete),
		Unchanged: capList(plan.Unchanged),
	}, truncated
}

func (s *Server) toolRewindWorkspace(rawArgs json.RawMessage) (string, bool, *rpcError) {
	var args struct {
		SessionID string `json:"session_id"`
		Step      int    `json:"step"`
		DryRun    bool   `json:"dry_run"`
	}
	if err := unmarshalArgs(rawArgs, &args); err != nil {
		return "", false, &rpcError{Code: -32602, Message: redact.String("invalid arguments for rewind_workspace: " + err.Error())}
	}
	if strings.TrimSpace(args.SessionID) == "" {
		return "", false, &rpcError{Code: -32602, Message: "session_id is required"}
	}
	if args.Step <= 0 {
		return "", false, &rpcError{Code: -32602, Message: "step must be >= 1"}
	}

	workDir := s.resolveWorkspaceDir()
	res, err := s.engine.RewindWithOptions(args.SessionID, args.Step, workDir, replay.RewindOptions{
		Force:  false,
		DryRun: args.DryRun,
	})
	if err != nil {
		return redact.String(err.Error()), true, nil
	}

	if !args.DryRun && res.SafetySnapshot == "" {
		return "mandatory pre-rewind safety savepoint was not created", true, nil
	}

	resp := map[string]any{
		"session_id":      redact.String(args.SessionID),
		"target_step":     res.TargetStep,
		"dry_run":         res.DryRun,
		"snapshot_hash":   res.SnapshotHash,
		"safety_snapshot": res.SafetySnapshot,
	}
	if res.Session != nil {
		resp["status"] = redact.String(res.Session.Status)
		resp["command"] = redact.String(res.Session.Command)
		resp["workspace_dir"] = redact.String(res.Session.WorkspaceDir)
		resp["remaining_frames"] = len(res.Session.Frames)
	}
	if res.Plan != nil {
		cleanPlan, planTruncated := redactAndCapPlan(res.Plan)
		resp["plan"] = cleanPlan
		if planTruncated {
			resp["plan_truncated"] = true
		}
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
