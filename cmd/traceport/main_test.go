package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/replay"
)

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("TRACEPORT_CLI_HELPER") != "1" {
		return
	}
	var args []string
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(0)
	}
	switch args[0] {
	case "ok":
		_ = os.WriteFile("created_by_agent.go", []byte("package demo\nconst X = 1\n"), 0644)
		fmt.Println("agent step completed")
		os.Exit(0)
	case "fail7":
		fmt.Fprintln(os.Stderr, "fatal error 7")
		os.Exit(7)
	case "secret_run":
		_ = os.WriteFile("audit_target.go", []byte("package demo\nconst Patched = true\n"), 0644)
		fmt.Println("using token " + "sk-proj-" + "SecretTestKey1234567890 and " + "ghp_" + "SecretGitHubToken123456")
		fmt.Fprintln(os.Stderr, "E_CONNECTION_REFUSED on https://admin:"+"superSecretPw99"+"@db.internal:5432/prod")
		os.Exit(0)
	}
	os.Exit(0)
}

func TestTruncateRunes_UTF8Safe(t *testing.T) {
	inputs := []string{
		"Çağdaş sistem mühendisliği çözümleri ve yüksek performanslı sıkıştırma",
		"日本語のマルチバイト文字列トランケーション検証テストケース",
		"Кириллический текст для проверки безопасной обрезки UTF-8",
	}
	for _, s := range inputs {
		for limit := 0; limit <= 35; limit++ {
			got := truncateRunes(s, limit)
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8 produced at limit %d: %q", limit, got)
			}
			if utf8.RuneCountInString(got) > limit {
				t.Fatalf("exceeded maxRunes %d: got %d runes", limit, utf8.RuneCountInString(got))
			}
		}
	}
}

func TestCLI_HelpVersionAndUnknownCommandExitCodes(t *testing.T) {
	var out, errBuf bytes.Buffer

	for _, flagArg := range [][]string{{}, {"--help"}, {"-h"}, {"help"}} {
		out.Reset()
		errBuf.Reset()
		if code := run(flagArg, &out, &errBuf); code != 0 {
			t.Fatalf("expected exit 0 for %v, got %d", flagArg, code)
		}
		if !strings.Contains(out.String(), "Usage: traceport") {
			t.Fatalf("expected usage output for %v", flagArg)
		}
	}

	origVer := Version
	defer func() { Version = origVer }()

	Version = "v1.1.0-custom"
	out.Reset()
	if code := run([]string{"--version"}, &out, &errBuf); code != 0 {
		t.Fatalf("version exit code %d", code)
	}
	if strings.TrimSpace(out.String()) != "traceport v1.1.0-custom" {
		t.Fatalf("unexpected version output: %q", out.String())
	}

	t.Setenv("TRACEPORT_HOME", t.TempDir())
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"nonexistent-subcommand"}, &out, &errBuf); code == 0 {
		t.Fatalf("expected non-zero exit code for unknown command")
	}
}

func TestCLI_EndToEndCommandsAndJSONFlags(t *testing.T) {
	t.Setenv("TRACEPORT_CLI_HELPER", "1")
	tpHome := t.TempDir()
	t.Setenv("TRACEPORT_HOME", tpHome)

	workDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, "initial.go"), []byte("package demo\nfunc Init() {}\n"), 0644)

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	var out, errBuf bytes.Buffer

	// 1. dict train --json
	if code := run([]string{"dict", "train", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("dict train failed (code=%d): %s", code, errBuf.String())
	}
	var dictMap map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &dictMap); err != nil {
		t.Fatalf("dict train --json invalid JSON: %v\n%s", err, out.String())
	}

	// 2. record successful helper command
	out.Reset()
	errBuf.Reset()
	code := run([]string{"record", "--", os.Args[0], "-test.run=TestCLIHelperProcess", "--", "ok"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("record ok failed (code=%d): %s", code, errBuf.String())
	}

	// 3. record failing helper command -> must propagate exact exit code 7!
	out.Reset()
	errBuf.Reset()
	code = run([]string{"record", "--", os.Args[0], "-test.run=TestCLIHelperProcess", "--", "fail7"}, &out, &errBuf)
	if code != 7 {
		t.Fatalf("expected propagated child exit code 7, got %d (stderr=%s)", code, errBuf.String())
	}

	// 4. sessions --json (alias check + JSON array check)
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"sessions", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("sessions --json failed: %s", errBuf.String())
	}
	var sessions []model.Session
	if err := json.Unmarshal(out.Bytes(), &sessions); err != nil || len(sessions) != 2 {
		t.Fatalf("expected 2 sessions in JSON output, got %d (err=%v)", len(sessions), err)
	}

	// Find the completed session ID (sessions[1], since sorted newest first).
	var okSessionID string
	for _, s := range sessions {
		if s.Status == "completed" {
			okSessionID = s.ID
		}
	}
	if okSessionID == "" {
		t.Fatalf("completed session not found")
	}

	// 5. inspect <id> --json
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"inspect", okSessionID, "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("inspect --json failed: %s", errBuf.String())
	}
	var inspected model.Session
	if err := json.Unmarshal(out.Bytes(), &inspected); err != nil || inspected.ID != okSessionID {
		t.Fatalf("inspect --json decode error: %v", err)
	}

	// 6. diff <id> --from 1 --to <last>
	out.Reset()
	errBuf.Reset()
	lastStep := fmt.Sprintf("%d", len(inspected.Frames))
	if code := run([]string{"diff", okSessionID, "--from", "1", "--to", lastStep}, &out, &errBuf); code != 0 {
		t.Fatalf("diff failed: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "created_by_agent.go") {
		t.Fatalf("expected created_by_agent.go in diff output:\n%s", out.String())
	}

	// 7. verify --json
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"verify", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("verify --json failed: %s", errBuf.String())
	}
	var vRep replay.VerifyReport
	if err := json.Unmarshal(out.Bytes(), &vRep); err != nil || !vRep.Valid {
		t.Fatalf("expected valid verify report, got %+v (err=%v)", vRep, err)
	}

	// 8. rewind --dry-run then live rewind to step 1
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"rewind", okSessionID, "--step", "1", "--dry-run"}, &out, &errBuf); code != 0 {
		t.Fatalf("rewind --dry-run failed: %s", errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(workDir, "created_by_agent.go")); err != nil {
		t.Fatalf("dry-run should not remove created_by_agent.go")
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"rewind", okSessionID, "--step", "1"}, &out, &errBuf); code != 0 {
		t.Fatalf("live rewind failed: %s", errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(workDir, "created_by_agent.go")); !os.IsNotExist(err) {
		t.Fatalf("live rewind to step 1 should have removed created_by_agent.go")
	}

	// 9. gc --keep 1 --json
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"gc", "--keep", "1", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("gc failed: %s", errBuf.String())
	}
	var gcRep replay.GCReport
	if err := json.Unmarshal(out.Bytes(), &gcRep); err != nil || gcRep.SessionsPruned != 1 {
		t.Fatalf("expected 1 pruned session in GC report, got %+v (err=%v)", gcRep, err)
	}
}

func TestCLI_TagsExportRedactAndSearch(t *testing.T) {
	t.Setenv("TRACEPORT_CLI_HELPER", "1")
	tpHome := t.TempDir()
	t.Setenv("TRACEPORT_HOME", tpHome)

	workDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, "base.go"), []byte("package demo\n"), 0644)

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	var out, errBuf bytes.Buffer

	// 1. record --tag bug-42 --tag sec,audit
	code := run([]string{
		"record", "--tag", "bug-42", "--tag", "sec,audit", "--",
		os.Args[0], "-test.run=TestCLIHelperProcess", "--", "secret_run",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("record --tag failed (code=%d): %s", code, errBuf.String())
	}

	// 2. record second untagged session
	out.Reset()
	errBuf.Reset()
	code = run([]string{
		"record", "--",
		os.Args[0], "-test.run=TestCLIHelperProcess", "--", "ok",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("record second session failed: %s", errBuf.String())
	}

	// 3. list --tag bug-42 --json
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"list", "--tag", "bug-42", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("list --tag failed: %s", errBuf.String())
	}
	var filtered []model.Session
	if err := json.Unmarshal(out.Bytes(), &filtered); err != nil || len(filtered) != 1 {
		t.Fatalf("expected 1 filtered session, got %d (err=%v)", len(filtered), err)
	}
	taggedID := filtered[0].ID

	// 4. tag add & remove
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"tag", taggedID, "prod-hotfix", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("tag add failed: %s", errBuf.String())
	}
	var afterAdd model.Session
	if err := json.Unmarshal(out.Bytes(), &afterAdd); err != nil || len(afterAdd.Tags) != 4 {
		t.Fatalf("expected 4 tags after add, got %+v (err=%v)", afterAdd.Tags, err)
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"tag", taggedID, "audit", "--remove", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("tag --remove failed: %s", errBuf.String())
	}
	var afterRm model.Session
	if err := json.Unmarshal(out.Bytes(), &afterRm); err != nil || len(afterRm.Tags) != 3 {
		t.Fatalf("expected 3 tags after remove, got %+v (err=%v)", afterRm.Tags, err)
	}

	// 5. export --format json --redact (stdout + --out file)
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"export", taggedID, "--format", "json", "--redact"}, &out, &errBuf); code != 0 {
		t.Fatalf("export json --redact failed: %s", errBuf.String())
	}
	rawJSONExport := out.String()
	for _, secret := range []string{"sk-proj-" + "SecretTestKey1234567890", "ghp_" + "SecretGitHubToken123456", "superSecret" + "Pw99"} {
		if strings.Contains(rawJSONExport, secret) {
			t.Fatalf("secret %q leaked in exported JSON:\n%s", secret, rawJSONExport)
		}
	}
	var portable model.Session
	if err := json.Unmarshal(out.Bytes(), &portable); err != nil || portable.ID != taggedID || len(portable.Frames) == 0 {
		t.Fatalf("exported portable JSON invalid: err=%v id=%q frames=%d", err, portable.ID, len(portable.Frames))
	}

	exportFile := filepath.Join(t.TempDir(), "audit", "report.md")
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"export", taggedID, "--format", "markdown", "--redact", "--out", exportFile}, &out, &errBuf); code != 0 {
		t.Fatalf("export markdown --out failed: %s", errBuf.String())
	}
	mdBytes, err := os.ReadFile(exportFile)
	if err != nil {
		t.Fatalf("read exported markdown file: %v", err)
	}
	mdText := string(mdBytes)
	if !strings.Contains(mdText, "## Session Metadata") || !strings.Contains(mdText, "audit_target.go") || !strings.Contains(mdText, "[REDACTED]") {
		t.Fatalf("unexpected markdown audit report content:\n%s", mdText)
	}
	if strings.Contains(mdText, "superSecretPw99") {
		t.Fatalf("secret leaked in markdown export")
	}

	// 6. search across checkpoint manifest path & regex error code
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"search", "audit_target.go", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("search file path failed: %s", errBuf.String())
	}
	var pathHits []replay.SessionSearchResult
	if err := json.Unmarshal(out.Bytes(), &pathHits); err != nil || len(pathHits) != 1 || pathHits[0].SessionID != taggedID {
		t.Fatalf("expected checkpoint_file search hit on %s, got %+v (err=%v)", taggedID, pathHits, err)
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"search", `E_CONNECTION_[A-Z]+`, "--regex", "--tag", "bug-42", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("search --regex --tag failed: %s", errBuf.String())
	}
	var regexHits []replay.SessionSearchResult
	if err := json.Unmarshal(out.Bytes(), &regexHits); err != nil || len(regexHits) != 1 {
		t.Fatalf("expected 1 regex search result, got %+v (err=%v)", regexHits, err)
	}
}
