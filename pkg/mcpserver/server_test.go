package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

func seedTestWorkspaceAndSession(t *testing.T) (*storage.Store, string, *model.Session, map[string]string) {
	t.Helper()

	storeDir := filepath.Join(t.TempDir(), "sessions")
	store, err := storage.NewStoreWithPath(storeDir)
	if err != nil {
		t.Fatalf("NewStoreWithPath: %v", err)
	}

	workDir := t.TempDir()
	ts, err := checkpoint.NewTreeStore(workDir, store.RootDir())
	if err != nil {
		t.Fatalf("NewTreeStore: %v", err)
	}
	defer ts.Close()

	// Step 1: initial clean workspace
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package main\nfunc Version() int { return 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	snap1, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatalf("CreateSnapshot 1: %v", err)
	}

	// Step 2 -> 3: mutate code AND write .env containing synthetic secrets!
	secrets := map[string]string{
		"openai":   "sk-proj-" + "McpTestSecretOpenAIKey987654",
		"github":   "ghp_" + "McpTestSecretGitHubToken123456",
		"envJwt":   "superSecretJwtSigningKey2026",
		"dbPass":   "prodDbPassword998877",
		"bearer":   "eyJhbGciOiJIUzI1NiJ9.mcpSecretPayload.sig",
	}

	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package main\nfunc Version() int { return 2 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	envBody := fmt.Sprintf("OPENAI_API_KEY=%s\nJWT_SECRET=%s\nDATABASE_PASSWORD=%s\n", secrets["openai"], secrets["envJwt"], secrets["dbPass"])
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte(envBody), 0600); err != nil {
		t.Fatal(err)
	}
	snap2, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatalf("CreateSnapshot 2: %v", err)
	}

	now := time.Now().UTC()
	sess := &model.Session{
		ID:           "trace_mcp_test_1",
		Command:      "OPENAI_API_KEY=" + secrets["openai"] + " go test ./...",
		WorkspaceDir: workDir,
		Tags:         []string{"agent-fix", "mcp"},
		StartTime:    now.Add(-5 * time.Second),
		EndTime:      now,
		Status:       "completed",
		ExitCode:     0,
		Frames: []model.Frame{
			{
				Index:       1,
				Timestamp:   now.Add(-5 * time.Second),
				Type:        model.EventCheckpoint,
				Content:     "Initial workspace snapshot",
				GitSnapshot: snap1,
			},
			{
				Index:     2,
				Timestamp: now.Add(-3 * time.Second),
				Type:      model.EventOutput,
				Content:   "Loaded token " + secrets["github"] + " with Authorization: Bearer " + secrets["bearer"],
			},
			{
				Index:       3,
				Timestamp:   now.Add(-2 * time.Second),
				Type:        model.EventCheckpoint,
				Content:     "After agent edit",
				GitSnapshot: snap2,
			},
			{
				Index:     4,
				Timestamp: now.Add(-1 * time.Second),
				Type:      model.EventError,
				Content:   "E_TIMEOUT connecting to https://admin:" + secrets["dbPass"] + "@db.internal:5432/app",
			},
		},
	}

	if err := store.SaveSession(sess); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	return store, workDir, sess, secrets
}

func runSingleRPC(t *testing.T, srv *Server, reqJSON string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	err := srv.ServeStdio(context.Background(), strings.NewReader(reqJSON+"\n"), &out)
	if err != nil {
		t.Fatalf("ServeStdio failed: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("invalid JSON-RPC response: %v\nraw: %s", err, out.String())
	}
	return resp
}

func extractToolText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	if errObj, hasErr := resp["error"]; hasErr && errObj != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", errObj)
	}
	res := resp["result"].(map[string]any)
	isErr, _ := res["isError"].(bool)
	content := res["content"].([]any)
	block := content[0].(map[string]any)
	return block["text"].(string), isErr
}

func TestMCPServer_ProtocolHandshakeFramingAndToolsList(t *testing.T) {
	store, workDir, _, _ := seedTestWorkspaceAndSession(t)
	srv := New(store, workDir, "1.3.0")

	// Test NDJSON stream with initialize, notification (must produce NO output!), ping, tools/list
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"list-1","method":"tools/list"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := srv.ServeStdio(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("ServeStdio NDJSON failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 responses (notification must be silent), got %d:\n%s", len(lines), out.String())
	}

	var initResp map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatal(err)
	}
	initRes := initResp["result"].(map[string]any)
	if initRes["protocolVersion"] != "2025-03-26" {
		t.Fatalf("unexpected protocolVersion: %v", initRes["protocolVersion"])
	}
	info := initRes["serverInfo"].(map[string]any)
	if info["name"] != "traceport" || info["version"] != "1.3.0" {
		t.Fatalf("unexpected serverInfo: %v", info)
	}

	var listResp map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &listResp); err != nil {
		t.Fatal(err)
	}
	tools := listResp["result"].(map[string]any)["tools"].([]any)
	wantTools := map[string]bool{
		"list_sessions":    false,
		"inspect_session":  false,
		"diff_checkpoints": false,
		"search_traces":    false,
		"rewind_workspace": false,
	}
	for _, rawTool := range tools {
		name := rawTool.(map[string]any)["name"].(string)
		if _, ok := wantTools[name]; ok {
			wantTools[name] = true
		}
	}
	for name, seen := range wantTools {
		if !seen {
			t.Fatalf("missing required MCP tool %q in tools/list", name)
		}
	}

	// Test Content-Length framing auto-detection!
	clBody := `{"jsonrpc":"2.0","id":99,"method":"ping"}`
	clReq := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(clBody), clBody)
	var clOut bytes.Buffer
	if err := srv.ServeStdio(context.Background(), strings.NewReader(clReq), &clOut); err != nil {
		t.Fatalf("ServeStdio Content-Length failed: %v", err)
	}
	if !strings.HasPrefix(clOut.String(), "Content-Length: ") {
		t.Fatalf("expected Content-Length framed reply, got: %q", clOut.String())
	}
}

func TestMCPServer_AllToolsAndZeroSecretLeakage(t *testing.T) {
	store, workDir, sess, secrets := seedTestWorkspaceAndSession(t)
	srv := New(store, workDir, "1.3.0")

	assertNoSecrets := func(toolName, payload string) {
		t.Helper()
		for k, sec := range secrets {
			if strings.Contains(payload, sec) {
				t.Fatalf("SECURITY FAILURE: %s leaked %s secret (%q)!\npayload:\n%s", toolName, k, sec, payload)
			}
		}
	}

	// 1. list_sessions
	listResp := runSingleRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions","arguments":{"tag":"mcp","limit":5}}}`)
	listText, isErr := extractToolText(t, listResp)
	if isErr {
		t.Fatalf("list_sessions returned tool error: %s", listText)
	}
	assertNoSecrets("list_sessions", listText)
	if !strings.Contains(listText, sess.ID) || !strings.Contains(listText, "[REDACTED]") {
		t.Fatalf("unexpected list_sessions payload:\n%s", listText)
	}

	// 2. inspect_session (with max_frames=2 truncation + redaction)
	inspResp := runSingleRPC(t, srv, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"inspect_session","arguments":{"session_id":%q,"max_frames":2}}}`, sess.ID))
	inspText, isErr := extractToolText(t, inspResp)
	if isErr {
		t.Fatalf("inspect_session returned tool error: %s", inspText)
	}
	assertNoSecrets("inspect_session", inspText)
	var inspObj map[string]any
	if err := json.Unmarshal([]byte(inspText), &inspObj); err != nil {
		t.Fatal(err)
	}
	if inspObj["truncated"] != true || int(inspObj["returned_frames"].(float64)) != 2 || int(inspObj["total_frames"].(float64)) != 4 {
		t.Fatalf("unexpected inspect_session truncation metadata: %+v", inspObj)
	}

	// 3. diff_checkpoints (step 1 -> step 3 MUST redact .env secrets!)
	diffResp := runSingleRPC(t, srv, fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"diff_checkpoints","arguments":{"session_id":%q,"from_step":1,"to_step":3}}}`, sess.ID))
	diffText, isErr := extractToolText(t, diffResp)
	if isErr {
		t.Fatalf("diff_checkpoints returned tool error: %s", diffText)
	}
	assertNoSecrets("diff_checkpoints", diffText)
	if !strings.Contains(diffText, ".env") || !strings.Contains(diffText, "main.go") || !strings.Contains(diffText, "[REDACTED]") {
		t.Fatalf("expected .env, main.go, and [REDACTED] in diff_checkpoints output:\n%s", diffText)
	}

	// 4. search_traces (regex + tag + redaction)
	searchResp := runSingleRPC(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_traces","arguments":{"query":"E_TIMEOUT|Loaded token","regex":true,"tag":"agent-fix","limit":10}}}`)
	searchText, isErr := extractToolText(t, searchResp)
	if isErr {
		t.Fatalf("search_traces returned tool error: %s", searchText)
	}
	assertNoSecrets("search_traces", searchText)
	if !strings.Contains(searchText, sess.ID) || !strings.Contains(searchText, "[REDACTED]") {
		t.Fatalf("unexpected search_traces output:\n%s", searchText)
	}

	// 5a. rewind_workspace with dry_run=true -> previews without modifying workspace!
	dryResp := runSingleRPC(t, srv, fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"rewind_workspace","arguments":{"session_id":%q,"step":1,"dry_run":true}}}`, sess.ID))
	dryText, isErr := extractToolText(t, dryResp)
	if isErr {
		t.Fatalf("rewind_workspace dry_run failed: %s", dryText)
	}
	assertNoSecrets("rewind_workspace(dry_run)", dryText)
	if _, err := os.Stat(filepath.Join(workDir, ".env")); err != nil {
		t.Fatalf("dry_run must NOT delete .env from workspace")
	}

	// 5b. rewind_workspace with dry_run=false -> creates mandatory pre-rewind safety savepoint and restores step 1!
	liveResp := runSingleRPC(t, srv, fmt.Sprintf(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"rewind_workspace","arguments":{"session_id":%q,"step":1,"dry_run":false}}}`, sess.ID))
	liveText, isErr := extractToolText(t, liveResp)
	if isErr {
		t.Fatalf("rewind_workspace live failed: %s", liveText)
	}
	assertNoSecrets("rewind_workspace(live)", liveText)
	var rwObj map[string]any
	if err := json.Unmarshal([]byte(liveText), &rwObj); err != nil {
		t.Fatal(err)
	}
	safetyHash, _ := rwObj["safety_snapshot"].(string)
	if len(safetyHash) != 64 {
		t.Fatalf("mandatory pre-rewind safety_snapshot missing or invalid: %q", safetyHash)
	}

	// Verify workspace actually rewound to step 1!
	if _, err := os.Stat(filepath.Join(workDir, ".env")); !os.IsNotExist(err) {
		t.Fatalf("expected .env to be removed after rewinding to step 1")
	}
	gotMain, err := os.ReadFile(filepath.Join(workDir, "main.go"))
	if err != nil || !strings.Contains(string(gotMain), "return 1") {
		t.Fatalf("expected main.go restored to Version() == 1, got %q (err=%v)", string(gotMain), err)
	}

	// Verify the mandatory safety savepoint exists in the shadow-tree store and preserved .env!
	ts, err := checkpoint.NewTreeStore(workDir, store.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if !ts.HasTree(safetyHash) {
		t.Fatalf("safety savepoint %s not found in shadow-tree store", safetyHash)
	}
}

func TestMCPServer_ErrorHandling(t *testing.T) {
	store, workDir, _, _ := seedTestWorkspaceAndSession(t)
	srv := New(store, workDir, "1.3.0")

	// Invalid JSON -> -32700
	resp := runSingleRPC(t, srv, `{broken-json`)
	if int(resp["error"].(map[string]any)["code"].(float64)) != -32700 {
		t.Fatalf("expected -32700 parse error, got %v", resp)
	}

	// Unknown method -> -32601
	resp = runSingleRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"does/not/exist"}`)
	if int(resp["error"].(map[string]any)["code"].(float64)) != -32601 {
		t.Fatalf("expected -32601 method not found, got %v", resp)
	}

	// Unknown tool -> -32602
	resp = runSingleRPC(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if int(resp["error"].(map[string]any)["code"].(float64)) != -32602 {
		t.Fatalf("expected -32602 invalid params for unknown tool, got %v", resp)
	}
}
