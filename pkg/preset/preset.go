// Package preset generates and atomically merges TracePort MCP server
// configurations for popular AI coding IDEs and CLIs (Claude Code, Cursor,
// VS Code, Windsurf) while preserving existing servers and tolerating JSONC.
package preset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Client identifies a supported MCP host IDE or CLI.
type Client string

const (
	ClientClaude   Client = "claude"
	ClientCursor   Client = "cursor"
	ClientVSCode   Client = "vscode"
	ClientWindsurf Client = "windsurf"
)

// SupportedClients returns all supported preset targets.
func SupportedClients() []Client {
	return []Client{
		ClientClaude,
		ClientCursor,
		ClientVSCode,
		ClientWindsurf,
	}
}

// ParseClient normalizes a user-supplied client name.
func ParseClient(raw string) (Client, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "claude", "claude-code", "claudecode":
		return ClientClaude, nil
	case "cursor":
		return ClientCursor, nil
	case "vscode", "vs-code", "code":
		return ClientVSCode, nil
	case "windsurf":
		return ClientWindsurf, nil
	default:
		return "", fmt.Errorf("unsupported MCP client %q (expected: claude, cursor, vscode, windsurf)", raw)
	}
}

// DefaultConfigPath resolves the default MCP config file path for client.
func DefaultConfigPath(client Client, homeDir string) (string, error) {
	if strings.TrimSpace(homeDir) == "" {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home directory: %w", err)
		}
	}

	switch client {
	case ClientClaude:
		return filepath.Join(homeDir, ".claude.json"), nil
	case ClientCursor:
		return filepath.Join(homeDir, ".cursor", "mcp.json"), nil
	case ClientVSCode:
		switch runtime.GOOS {
		case "windows":
			appData := strings.TrimSpace(os.Getenv("APPDATA"))
			if appData == "" {
				appData = filepath.Join(homeDir, "AppData", "Roaming")
			}
			return filepath.Join(appData, "Code", "User", "mcp.json"), nil
		case "darwin":
			return filepath.Join(homeDir, "Library", "Application Support", "Code", "User", "mcp.json"), nil
		default:
			return filepath.Join(homeDir, ".config", "Code", "User", "mcp.json"), nil
		}
	case ClientWindsurf:
		return filepath.Join(homeDir, ".codeium", "windsurf", "mcp_config.json"), nil
	default:
		return "", fmt.Errorf("unsupported client %q", client)
	}
}

// BuildMergedConfig merges the "traceport" MCP server entry into existingData
// (which may be empty or contain JSONC comments and trailing commas), preserving
// all existing top-level fields and other configured MCP servers.
func BuildMergedConfig(client Client, binaryPath string, existingData []byte) ([]byte, error) {
	bin := strings.TrimSpace(binaryPath)
	if bin == "" {
		bin = "traceport"
	}

	root := make(map[string]any)
	cleaned := strings.TrimSpace(string(stripJSONC(existingData)))
	if cleaned != "" {
		if err := json.Unmarshal([]byte(cleaned), &root); err != nil {
			return nil, fmt.Errorf("parse existing %s config: %w", client, err)
		}
		if root == nil {
			root = make(map[string]any)
		}
	}

	switch client {
	case ClientVSCode:
		servers, err := ensureObjectMap(root, "servers")
		if err != nil {
			return nil, err
		}
		servers["traceport"] = map[string]any{
			"type":    "stdio",
			"command": bin,
			"args":    []string{"mcp"},
		}
	case ClientClaude, ClientCursor, ClientWindsurf:
		servers, err := ensureObjectMap(root, "mcpServers")
		if err != nil {
			return nil, err
		}
		servers["traceport"] = map[string]any{
			"command": bin,
			"args":    []string{"mcp"},
		}
	default:
		return nil, fmt.Errorf("unsupported client %q", client)
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal %s config: %w", client, err)
	}
	return append(out, '\n'), nil
}

// Preview computes the merged MCP JSON config for configPath (reading any existing file if present)
// without modifying disk.
func Preview(client Client, configPath, binaryPath string) ([]byte, error) {
	var existing []byte
	if strings.TrimSpace(configPath) != "" {
		data, err := os.ReadFile(configPath)
		if err == nil {
			existing = data
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read existing config %q: %w", configPath, err)
		}
	}
	return BuildMergedConfig(client, binaryPath, existing)
}

// Apply merges and atomically writes the TracePort MCP configuration to configPath.
func Apply(client Client, configPath, binaryPath string) ([]byte, error) {
	if strings.TrimSpace(configPath) == "" {
		return nil, fmt.Errorf("config path must not be empty")
	}

	var existing []byte
	var existingPerm os.FileMode = 0600
	if info, statErr := os.Stat(configPath); statErr == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("config path %q is a directory", configPath)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0 {
			existingPerm = info.Mode().Perm()
		}
		data, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("read existing config %q: %w", configPath, err)
		}
		existing = data
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("stat config %q: %w", configPath, statErr)
	}

	merged, err := BuildMergedConfig(client, binaryPath, existing)
	if err != nil {
		return nil, err
	}

	if err := writeAtomicPresetFile(configPath, merged, existingPerm); err != nil {
		return nil, err
	}
	return merged, nil
}

func ensureObjectMap(parent map[string]any, key string) (map[string]any, error) {
	raw, exists := parent[key]
	if !exists || raw == nil {
		m := make(map[string]any)
		parent[key] = m
		return m, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("existing field %q is not a JSON object", key)
	}
	return m, nil
}

func writeAtomicPresetFile(dest string, data []byte, perm os.FileMode) error {
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	dir := filepath.Dir(absDest)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".tp-preset-*")
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

	var renameErr error
	for attempt := 0; attempt <= 5; attempt++ {
		if attempt > 0 {
			_ = os.Chmod(absDest, 0600)
			time.Sleep(25 * time.Millisecond)
		}
		if renameErr = os.Rename(tmpName, absDest); renameErr == nil {
			break
		}
	}
	if renameErr != nil {
		return renameErr
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(absDest, perm)
	}
	cleanup = false
	return nil
}

// stripJSONC strips // line comments, /* */ block comments, and trailing commas
// outside JSON string literals so VS Code / Cursor JSONC configs parse cleanly.
func stripJSONC(src []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(src))
	inString := false
	escaped := false

	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			out.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			out.WriteByte(c)
			continue
		}

		if c == '/' && i+1 < len(src) {
			if src[i+1] == '/' {
				i += 2
				for i < len(src) && src[i] != '\n' {
					i++
				}
				if i < len(src) {
					out.WriteByte('\n')
				}
				continue
			}
			if src[i+1] == '*' {
				i += 2
				for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
					i++
				}
				i++
				continue
			}
		}

		out.WriteByte(c)
	}

	clean := out.Bytes()
	var final bytes.Buffer
	final.Grow(len(clean))
	inString = false
	escaped = false
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		if inString {
			final.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			final.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(clean) && (clean[j] == ' ' || clean[j] == '\t' || clean[j] == '\r' || clean[j] == '\n') {
				j++
			}
			if j < len(clean) && (clean[j] == '}' || clean[j] == ']') {
				continue
			}
		}
		final.WriteByte(c)
	}

	return final.Bytes()
}
