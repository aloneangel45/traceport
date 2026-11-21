package replay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/storage"
)

func TestExportHTML_SelfContainedDiffXSSAndRedaction(t *testing.T) {
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

	// Snapshot 1
	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\nconst Mode = \"old\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	snap1, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot 2: modify app.go with XSS payload AND add .env with secrets
	xssPayload := `<script>alert('xss')</script><img src=x onerror=alert(1)>`
	secretOpenAI := "sk-proj-" + "HtmlExportSecretKey123456789"
	secretGitHub := "ghp_" + "HtmlExportGitHubToken987654"
	secretEnv := "ultraSecretJwtKeyHtml2026"
	secretBearer := "eyJhbGciOiJIUzI1NiJ9.htmlSecret.sig"

	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\nconst Mode = \"new\"\n// "+xssPayload+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("OPENAI_API_KEY="+secretOpenAI+"\nJWT_SECRET="+secretEnv+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snap2, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	sess := &model.Session{
		ID:           "trace_html_test_1",
		Command:      `echo "` + xssPayload + `" && export KEY=` + secretOpenAI,
		WorkspaceDir: workDir,
		Tags:         []string{"html-audit", "security"},
		StartTime:    now.Add(-4 * time.Second),
		EndTime:      now,
		Status:       "completed",
		ExitCode:     0,
		Frames: []model.Frame{
			{
				Index:       1,
				Timestamp:   now.Add(-4 * time.Second),
				Type:        model.EventCheckpoint,
				Content:     "Initial workspace snapshot",
				GitSnapshot: snap1,
			},
			{
				Index:     2,
				Timestamp: now.Add(-3 * time.Second),
				Type:      model.EventOutput,
				Content:   "stdout line " + xssPayload + " token=" + secretGitHub,
			},
			{
				Index:     3,
				Timestamp: now.Add(-2 * time.Second),
				Type:      model.EventToolCall,
				Content:   "invoking patch_file",
				Tool: &model.ToolInvocation{
					Name: "patch_file",
					Arguments: map[string]any{
						"authorization": "Bearer " + secretBearer,
						"note":          xssPayload,
					},
					Result:     "patched ok </script><svg onload=alert(2)>",
					DurationMs: 42,
				},
			},
			{
				Index:       4,
				Timestamp:   now.Add(-1 * time.Second),
				Type:        model.EventCheckpoint,
				Content:     "Post-patch checkpoint",
				GitSnapshot: snap2,
			},
			{
				Index:     5,
				Timestamp: now,
				Type:      model.EventError,
				Content:   "stderr failure: " + xssPayload,
			},
		},
	}
	if err := store.SaveSession(sess); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	eng := NewEngine(store)
	// Intentionally pass Redact: false to prove HTML export ALWAYS redacts secrets!
	rawHTML, err := eng.ExportSession(sess.ID, ExportOptions{
		Format: "html",
		Redact: false,
	})
	if err != nil {
		t.Fatalf("ExportSession(html) failed: %v", err)
	}
	doc := string(rawHTML)

	// 1. Zero external CDN dependencies
	for _, forbiddenCDN := range []string{"<link ", "cdn.", "unpkg.com", "jsdelivr.net", "fonts.googleapis.com", "<script src="} {
		if strings.Contains(strings.ToLower(doc), forbiddenCDN) {
			t.Fatalf("HTML export must be self-contained with zero external CDNs, found %q", forbiddenCDN)
		}
	}

	// 2. Verify header, filter buttons, and syntax-colored unified diff classes
	for _, mustHave := range []string{
		"<!DOCTYPE html>",
		"trace_html_test_1",
		"#html-audit",
		`data-filter="all"`,
		`data-filter="checkpoint"`,
		`data-filter="output"`,
		`data-filter="error"`,
		`data-filter="tool_call"`,
		`class="diff-line diff-add"`,
		`class="diff-line diff-del"`,
		`class="diff-line diff-hunk"`,
		"[REDACTED]",
		"&lt;script&gt;alert(&#39;xss&#39;)&lt;/script&gt;",
	} {
		if !strings.Contains(doc, mustHave) {
			t.Fatalf("HTML report missing expected content %q", mustHave)
		}
	}

	// 3. Zero XSS injection anywhere in document
	for _, rawXSS := range []string{
		`<script>alert('xss')</script>`,
		`<img src=x onerror=alert(1)>`,
		`<svg onload=alert(2)>`,
	} {
		if strings.Contains(doc, rawXSS) {
			t.Fatalf("XSS VULNERABILITY: unescaped payload %q appeared in HTML export!", rawXSS)
		}
	}

	// 4. Zero secret leakage across command, frames, tool calls, and checkpoint diffs
	for _, sec := range []string{secretOpenAI, secretGitHub, secretEnv, secretBearer} {
		if strings.Contains(doc, sec) {
			t.Fatalf("SECRET LEAK: raw secret %q appeared in HTML export!", sec)
		}
	}
}
