package preset

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseClient(t *testing.T) {
	cases := map[string]Client{
		"claude":      ClientClaude,
		"CLAUDE-CODE": ClientClaude,
		"claudecode":  ClientClaude,
		"cursor":      ClientCursor,
		"vscode":      ClientVSCode,
		"vs-code":     ClientVSCode,
		"code":        ClientVSCode,
		"windsurf":    ClientWindsurf,
	}
	for raw, want := range cases {
		got, err := ParseClient(raw)
		if err != nil {
			t.Fatalf("ParseClient(%q) unexpected error: %v", raw, err)
		}
		if got != want {
			t.Fatalf("ParseClient(%q) = %q, want %q", raw, got, want)
		}
	}
	if _, err := ParseClient("unknown-ide"); err == nil {
		t.Fatalf("expected error for unsupported client")
	}
	if len(SupportedClients()) != 4 {
		t.Fatalf("expected 4 supported clients, got %d", len(SupportedClients()))
	}
}

func TestDefaultConfigPath(t *testing.T) {
	home := t.TempDir()
	for _, client := range SupportedClients() {
		p, err := DefaultConfigPath(client, home)
		if err != nil {
			t.Fatalf("DefaultConfigPath(%s) failed: %v", client, err)
		}
		if p == "" {
			t.Fatalf("DefaultConfigPath(%s) returned empty path", client)
		}
	}
}

func TestBuildMergedConfig_PreservesExistingServersAndJSONC(t *testing.T) {
	existing := []byte(`{
  // User comment
  "theme": "obsidian",
  /* Block comment with // nested */
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "URL": "https://example.com//keep-slashes"
      },
    },
  },
}`)

	merged, err := BuildMergedConfig(ClientClaude, "/usr/local/bin/traceport", existing)
	if err != nil {
		t.Fatalf("BuildMergedConfig failed: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(merged, &parsed); err != nil {
		t.Fatalf("merged JSON invalid: %v\n%s", err, string(merged))
	}
	if parsed["theme"] != "obsidian" {
		t.Fatalf("existing top-level key 'theme' was lost")
	}
	servers, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("missing mcpServers map")
	}
	if _, ok := servers["github"]; !ok {
		t.Fatalf("existing 'github' server was clobbered")
	}
	gh := servers["github"].(map[string]any)
	ghEnv := gh["env"].(map[string]any)
	if ghEnv["URL"] != "https://example.com//keep-slashes" {
		t.Fatalf("URL inside string literal was corrupted by JSONC stripper: %v", ghEnv["URL"])
	}

	tp, ok := servers["traceport"].(map[string]any)
	if !ok {
		t.Fatalf("missing traceport entry")
	}
	if tp["command"] != "/usr/local/bin/traceport" {
		t.Fatalf("unexpected traceport command: %v", tp["command"])
	}
	args := tp["args"].([]any)
	if len(args) != 1 || args[0] != "mcp" {
		t.Fatalf("unexpected traceport args: %v", args)
	}
}

func TestBuildMergedConfig_VSCodeUsesServersAndStdio(t *testing.T) {
	merged, err := BuildMergedConfig(ClientVSCode, "traceport", nil)
	if err != nil {
		t.Fatalf("BuildMergedConfig(vscode) failed: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(merged, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	servers, ok := parsed["servers"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'servers' key for VS Code, got %v", parsed)
	}
	tp := servers["traceport"].(map[string]any)
	if tp["type"] != "stdio" || tp["command"] != "traceport" {
		t.Fatalf("unexpected vscode traceport entry: %v", tp)
	}
}

func TestBuildMergedConfig_RejectsCorruptContainer(t *testing.T) {
	corrupt := []byte(`{"mcpServers": ["not-an-object"]}`)
	if _, err := BuildMergedConfig(ClientCursor, "traceport", corrupt); err == nil {
		t.Fatalf("expected error when mcpServers is not a JSON object")
	}
}

func TestApplyAndPreview_AtomicWriteAndPreserve(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nested", "mcp.json")

	// Preview on non-existent file must NOT create file.
	prev, err := Preview(ClientWindsurf, cfgPath, "/bin/traceport")
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}
	if !strings.Contains(string(prev), `"traceport"`) {
		t.Fatalf("Preview missing traceport entry: %s", string(prev))
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("Preview must not create config file on disk")
	}

	// Seed existing config with another server.
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatal(err)
	}
	initial := `{"otherKey": 42, "mcpServers": {"existing": {"command": "foo", "args": ["bar"]}}}`
	if err := os.WriteFile(cfgPath, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}

	// Apply must atomically merge while preserving existing server and otherKey.
	written, err := Apply(ClientWindsurf, cfgPath, "/opt/traceport")
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	onDisk, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config after Apply: %v", err)
	}
	if string(onDisk) != string(written) {
		t.Fatalf("on-disk content differs from returned merged bytes")
	}
	var parsed map[string]any
	if err := json.Unmarshal(onDisk, &parsed); err != nil {
		t.Fatalf("invalid JSON written: %v", err)
	}
	if parsed["otherKey"].(float64) != 42 {
		t.Fatalf("lost otherKey after Apply")
	}
	servers := parsed["mcpServers"].(map[string]any)
	if _, ok := servers["existing"]; !ok {
		t.Fatalf("lost existing MCP server after Apply")
	}
	if _, ok := servers["traceport"]; !ok {
		t.Fatalf("missing traceport MCP server after Apply")
	}

	// Ensure no leftover temp files in directory.
	entries, err := os.ReadDir(filepath.Dir(cfgPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tp-preset-") {
			t.Fatalf("leftover temp file found: %s", e.Name())
		}
	}
}

func TestBuildMergedConfig_URLsBOMLargeIntsHTMLCharsAndUnterminatedComment(t *testing.T) {
	// Include UTF-8 BOM, http:// and https:// URLs with query &, block-comment tokens inside strings,
	// escaped quotes with //, 64-bit integer precision, and trailing commas.
	existing := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{
  // Line comment with http://ignored.example
  "bigTimestamp": 1794348120984102999,
  "mcpServers": {
    "remote": {
      "url": "http://localhost:8080/mcp?a=1&b=2&tag=<prod>",
      "note": "keep /* not a comment */ and \" // still inside string",
      "winPath": "C:\\Users\\Dev\\", // trailing comment after escaped backslash!
    },
  },
}`)...)

	merged, err := BuildMergedConfig(ClientCursor, "/usr/local/bin/traceport", existing)
	if err != nil {
		t.Fatalf("BuildMergedConfig with BOM & URLs failed: %v", err)
	}

	mergedStr := string(merged)
	if !strings.Contains(mergedStr, `"bigTimestamp": 1794348120984102999`) {
		t.Fatalf("64-bit integer precision was corrupted:\n%s", mergedStr)
	}
	if !strings.Contains(mergedStr, `"http://localhost:8080/mcp?a=1&b=2&tag=<prod>"`) {
		t.Fatalf("URL or HTML characters (&, <, >) were corrupted:\n%s", mergedStr)
	}
	if strings.Contains(mergedStr, `\u0026`) || strings.Contains(mergedStr, `\u003c`) {
		t.Fatalf("unexpected HTML escaping in merged JSON config:\n%s", mergedStr)
	}

	var parsed map[string]any
	if err := json.Unmarshal(merged, &parsed); err != nil {
		t.Fatalf("merged JSON invalid: %v\n%s", err, mergedStr)
	}
	remote := parsed["mcpServers"].(map[string]any)["remote"].(map[string]any)
	if remote["note"] != `keep /* not a comment */ and " // still inside string` {
		t.Fatalf("string literal containing /* and // was corrupted: %q", remote["note"])
	}
	if remote["winPath"] != `C:\Users\Dev\` {
		t.Fatalf("escaped backslash before closing quote was corrupted: %q", remote["winPath"])
	}

	// Unterminated block comment and trailing garbage MUST return errors.
	if _, err := BuildMergedConfig(ClientClaude, "traceport", []byte(`{"a": 1} /* unterminated`)); err == nil {
		t.Fatalf("expected error for unterminated block comment")
	}
	if _, err := BuildMergedConfig(ClientClaude, "traceport", []byte(`{"a": 1} extra`)); err == nil {
		t.Fatalf("expected error for trailing garbage after JSON object")
	}
}
