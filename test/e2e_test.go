package test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aloneangel45/traceport/pkg/model"
	"github.com/aloneangel45/traceport/pkg/recorder"
	"github.com/aloneangel45/traceport/pkg/replay"
	"github.com/aloneangel45/traceport/pkg/storage"
)

func TestTracePort_LiveRecordAndReplay(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := storage.NewStoreWithPath(tmpDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	runner := recorder.NewRunner(store, tmpDir)
	if err := runner.SetTags([]string{"e2e", "live-test"}); err != nil {
		t.Fatalf("SetTags failed: %v", err)
	}

	cmdName := "cmd.exe"
	cmdArgs := []string{"/c", "echo", "HELLO_TRACEPORT_TEST"}
	if runtime.GOOS != "windows" {
		cmdName = "sh"
		cmdArgs = []string{"-c", "echo HELLO_TRACEPORT_TEST"}
	}

	session, err := runner.Record(cmdName, cmdArgs)
	if err != nil {
		t.Fatalf("record failed: %v", err)
	}

	if session.Status != "completed" {
		t.Errorf("expected session completed, got %s", session.Status)
	}
	if len(session.Frames) == 0 {
		t.Errorf("expected at least 1 recorded frame")
	}
	if !storage.HasTag(*session, "e2e") || !storage.HasTag(*session, "live-test") {
		t.Errorf("expected recorded tags [e2e live-test], got %v", session.Tags)
	}

	eng := replay.NewEngine(store)
	var out bytes.Buffer
	if err := eng.Playback(session.ID, 1, &out); err != nil {
		t.Fatalf("playback failed: %v", err)
	}

	if !bytes.Contains(out.Bytes(), []byte("HELLO_TRACEPORT_TEST")) {
		t.Errorf("expected replay to contain command output, got: %s", out.String())
	}
}

func TestTracePort_CompiledBinaryFullLifecycle(t *testing.T) {
	binExt := ""
	if runtime.GOOS == "windows" {
		binExt = ".exe"
	}
	binPath := filepath.Join(t.TempDir(), "traceport"+binExt)

	buildCmd := exec.Command("go", "build", "-ldflags=-X main.Version=1.2.0-e2e", "-o", binPath, "../cmd/traceport")
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build traceport binary with CGO_ENABLED=0: %v\n%s", err, string(out))
	}

	tpHome := t.TempDir()
	workDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, "service.go"), []byte("package service\nconst Version = 1\n"), 0644)

	runTP := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(binPath, args...)
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(), "TRACEPORT_HOME="+tpHome)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// 1. Verify ldflags version injection.
	verOut, err := runTP("version")
	if err != nil || strings.TrimSpace(verOut) != "traceport v1.2.0-e2e" {
		t.Fatalf("unexpected version output: %q (err=%v)", verOut, err)
	}

	// 2. Train workspace dictionary.
	dictOut, err := runTP("dict", "train")
	if err != nil || !strings.Contains(dictOut, "[ok] trained zstd dictionary") {
		t.Fatalf("dict train failed: %v\n%s", err, dictOut)
	}

	// 3. Record command with --tag that mutates workspace and emits a synthetic credential.
	synthKey := "sk-proj-" + "TestSecretCredential987654"
	var recArgs []string
	if runtime.GOOS == "windows" {
		recArgs = []string{"record", "--tag", "bug-42,ci", "--", "cmd.exe", "/c", "echo package service > generated.go && echo KEY=" + synthKey + " && echo DONE_STEP"}
	} else {
		recArgs = []string{"record", "--tag", "bug-42,ci", "--", "sh", "-c", "printf 'package service\n' > generated.go && echo KEY=" + synthKey + " && echo DONE_STEP"}
	}
	recOut, err := runTP(recArgs...)
	if err != nil {
		t.Fatalf("binary record --tag failed: %v\n%s", err, recOut)
	}

	// 4. Record a second untagged session so list --tag filtering is verified.
	var recArgs2 []string
	if runtime.GOOS == "windows" {
		recArgs2 = []string{"record", "--", "cmd.exe", "/c", "echo SECOND_UNFILTERED_RUN"}
	} else {
		recArgs2 = []string{"record", "--", "sh", "-c", "echo SECOND_UNFILTERED_RUN"}
	}
	if out2, err2 := runTP(recArgs2...); err2 != nil {
		t.Fatalf("second record failed: %v\n%s", err2, out2)
	}

	// 5. Filter sessions with list --tag bug-42 --json and table output.
	listTableOut, err := runTP("list", "--tag", "bug-42")
	if err != nil || !strings.Contains(listTableOut, "bug-42") {
		t.Fatalf("list --tag table failed: %v\n%s", err, listTableOut)
	}

	listJSONOut, err := runTP("list", "--tag", "bug-42", "--json")
	if err != nil {
		t.Fatalf("list --tag --json failed: %v\n%s", err, listJSONOut)
	}
	var taggedSessions []model.Session
	if err := json.Unmarshal([]byte(listJSONOut), &taggedSessions); err != nil || len(taggedSessions) != 1 {
		t.Fatalf("expected exactly 1 session matching bug-42, got %d (err=%v)\n%s", len(taggedSessions), err, listJSONOut)
	}
	sessionID := taggedSessions[0].ID

	// 6. Add and remove tags via `traceport tag`.
	tagAddOut, err := runTP("tag", sessionID, "release-blocker", "--json")
	if err != nil {
		t.Fatalf("tag add failed: %v\n%s", err, tagAddOut)
	}
	var afterTagAdd model.Session
	if err := json.Unmarshal([]byte(tagAddOut), &afterTagAdd); err != nil || !storage.HasTag(afterTagAdd, "release-blocker") {
		t.Fatalf("expected release-blocker tag on session: %+v (err=%v)", afterTagAdd.Tags, err)
	}

	tagRmOut, err := runTP("tag", sessionID, "ci", "--remove", "--json")
	if err != nil {
		t.Fatalf("tag --remove failed: %v\n%s", err, tagRmOut)
	}
	var afterTagRm model.Session
	if err := json.Unmarshal([]byte(tagRmOut), &afterTagRm); err != nil || storage.HasTag(afterTagRm, "ci") {
		t.Fatalf("expected ci tag removed: %+v (err=%v)", afterTagRm.Tags, err)
	}

	// 7. Portable JSON export with --redact (stdout + --out).
	exportJSONPath := filepath.Join(t.TempDir(), "portable_session.json")
	exportJSONCmdOut, err := runTP("export", sessionID, "--format", "json", "--redact", "--out", exportJSONPath)
	if err != nil {
		t.Fatalf("export --format json --redact --out failed: %v\n%s", err, exportJSONCmdOut)
	}
	exportedJSONBytes, err := os.ReadFile(exportJSONPath)
	if err != nil {
		t.Fatalf("failed reading exported JSON file: %v", err)
	}
	if strings.Contains(string(exportedJSONBytes), synthKey) {
		t.Fatalf("secret credential leaked in exported JSON:\n%s", string(exportedJSONBytes))
	}
	var exportedSess model.Session
	if err := json.Unmarshal(exportedJSONBytes, &exportedSess); err != nil || exportedSess.ID != sessionID || len(exportedSess.Frames) == 0 {
		t.Fatalf("invalid portable exported session JSON: err=%v session=%+v", err, exportedSess)
	}

	// 8. Markdown flight audit report export (--format markdown --redact).
	exportMDOut, err := runTP("export", sessionID, "--format", "markdown", "--redact")
	if err != nil {
		t.Fatalf("export --format markdown failed: %v\n%s", err, exportMDOut)
	}
	for _, mustHave := range []string{"# TracePort Flight Audit Report", "## Session Metadata", "generated.go", "[REDACTED]"} {
		if !strings.Contains(exportMDOut, mustHave) {
			t.Fatalf("markdown audit report missing %q:\n%s", mustHave, exportMDOut)
		}
	}
	if strings.Contains(exportMDOut, synthKey) {
		t.Fatalf("secret credential leaked in markdown audit report")
	}

	// 9. Cross-session search (checkpoint manifest file path + regex + tag filter + JSON).
	searchFileOut, err := runTP("search", "generated.go", "--json")
	if err != nil {
		t.Fatalf("search generated.go failed: %v\n%s", err, searchFileOut)
	}
	var fileSearchResults []replay.SessionSearchResult
	if err := json.Unmarshal([]byte(searchFileOut), &fileSearchResults); err != nil || len(fileSearchResults) != 1 || fileSearchResults[0].SessionID != sessionID {
		t.Fatalf("unexpected search results for generated.go: %+v (err=%v)", fileSearchResults, err)
	}

	searchRegexOut, err := runTP("search", "DONE_[A-Z]+", "--regex", "--tag", "bug-42")
	if err != nil || !strings.Contains(searchRegexOut, sessionID) {
		t.Fatalf("search --regex --tag failed: %v\n%s", err, searchRegexOut)
	}

	// 10. Verify cryptographic integrity across all sessions.
	verStoreOut, err := runTP("verify")
	if err != nil || !strings.Contains(verStoreOut, "[ok] verified 2 sessions") {
		t.Fatalf("binary verify failed: %v\n%s", err, verStoreOut)
	}
}

func TestInstallerPS1_RestrictedPSModulePathEndToEnd(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 integration test runs on Windows")
	}

	psExe, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe not found in PATH")
	}

	builtBin := filepath.Join(t.TempDir(), "traceport.exe")
	buildCmd := exec.Command("go", "build", "-o", builtBin, "../cmd/traceport")
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed building traceport.exe: %v\n%s", err, string(out))
	}
	binBytes, err := os.ReadFile(builtBin)
	if err != nil {
		t.Fatal(err)
	}

	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	archiveName := "traceport-windows-" + arch + ".zip"

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	// Nest binary inside a subdirectory to verify recursive extraction lookup too.
	fw, err := zw.Create("release-bundle/traceport-windows-" + arch + ".exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(binBytes); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipPayload := zipBuf.Bytes()
	sum := sha256.Sum256(zipPayload)
	validSHA := hex.EncodeToString(sum[:])

	serveTampered := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + archiveName:
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(zipPayload)
		case "/checksums.txt":
			digest := validSHA
			if serveTampered {
				digest = strings.Repeat("0", 64)
			}
			_, _ = fmt.Fprintf(w, "%s *%s\n", digest, archiveName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	scriptPath, err := filepath.Abs("../install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Pre-populate a read-only old binary to ensure install.ps1 clears FILE_ATTRIBUTE_READONLY.
	destExe := filepath.Join(installDir, "traceport.exe")
	if err := os.WriteFile(destExe, []byte("stale-binary"), 0444); err != nil {
		t.Fatal(err)
	}

	runInstaller := func() (string, error) {
		psBootstrap := fmt.Sprintf(
			"$PSModuleAutoLoadingPreference = 'None'; $env:PSModulePath = ''; & '%s'",
			strings.ReplaceAll(scriptPath, "'", "''"),
		)
		cmd := exec.Command(psExe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psBootstrap)
		var cleanEnv []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(strings.ToUpper(kv), "PSMODULEPATH=") {
				continue
			}
			cleanEnv = append(cleanEnv, kv)
		}
		cleanEnv = append(cleanEnv,
			"PSModulePath=",
			"TRACEPORT_INSTALL_DIR="+installDir,
			"TRACEPORT_RELEASE_BASE_URL="+srv.URL,
			"TRACEPORT_SKIP_PATH_UPDATE=1",
		)
		cmd.Env = cleanEnv
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// 1. Valid SHA-256 + zero-module PowerShell must succeed and overwrite read-only binary.
	out, err := runInstaller()
	if err != nil {
		t.Fatalf("install.ps1 failed under restricted PSModulePath: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[ok] verified SHA-256 digest: "+validSHA) {
		t.Fatalf("expected verified SHA-256 log in installer output:\n%s", out)
	}

	verOut, err := exec.Command(destExe, "version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(verOut)) != "traceport v1.3.0" {
		t.Fatalf("installed traceport.exe version check failed: err=%v out=%q", err, string(verOut))
	}

	// 2. Tampered checksums.txt must fail closed.
	serveTampered = true
	badOut, badErr := runInstaller()
	if badErr == nil || !strings.Contains(badOut, "SHA-256 checksum mismatch") {
		t.Fatalf("expected SHA-256 mismatch error from install.ps1, got err=%v\n%s", badErr, badOut)
	}

	// 3. Verify Desktop (5.1) PSModulePath strips PowerShell\7 entries, prepends $PSHome\Modules,
	//    and `irm | iex` style invocation throws cleanly without killing the caller shell via `exit`.
	psIexCheck := fmt.Sprintf(
		`$env:PSModulePath = 'C:\Program Files\PowerShell\7\Modules;C:\Custom\Modules'; `+
			`$caught = $false; `+
			`try { Invoke-Expression ([System.IO.File]::ReadAllText('%s')) } catch { $caught = $true }; `+
			`Write-Output ("AFTER_IEX_ALIVE=" + $caught); `+
			`Write-Output ("FINAL_PSMP=" + $env:PSModulePath)`,
		strings.ReplaceAll(scriptPath, "'", "''"),
	)
	iexCmd := exec.Command(psExe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psIexCheck)
	iexCmd.Env = append(os.Environ(),
		"TRACEPORT_INSTALL_DIR="+installDir,
		"TRACEPORT_RELEASE_BASE_URL="+srv.URL,
		"TRACEPORT_SKIP_PATH_UPDATE=1",
	)
	iexOutBytes, iexErr := iexCmd.CombinedOutput()
	iexOut := string(iexOutBytes)
	if iexErr != nil {
		t.Fatalf("caller shell exited unexpectedly during iex of install.ps1: %v\n%s", iexErr, iexOut)
	}
	if !strings.Contains(iexOut, "AFTER_IEX_ALIVE=True") {
		t.Fatalf("expected iex failure to be catchable without terminating caller shell:\n%s", iexOut)
	}
	if strings.Contains(strings.ToLower(iexOut), `powershell\7`) {
		t.Fatalf("expected PowerShell\\7 module path stripped in Desktop 5.1 edition:\n%s", iexOut)
	}
	if !strings.Contains(iexOut, `C:\Custom\Modules`) {
		t.Fatalf("expected non-PS7 custom module path preserved:\n%s", iexOut)
	}
}

func TestInstallerSHAndRepoConfigs_PureLFAndAtomicInstall(t *testing.T) {
	lfFiles := []string{
		"../install.sh",
		"../.github/workflows/ci.yml",
		"../.github/workflows/release.yml",
	}
	for _, rel := range lfFiles {
		raw, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if bytes.Contains(raw, []byte{'\r'}) {
			t.Fatalf("%s contains CR (\\r) bytes; must be pure LF (\\n)!", rel)
		}
	}

	gitAttrBytes, err := os.ReadFile("../.gitattributes")
	if err != nil {
		t.Fatalf("read ../.gitattributes: %v", err)
	}
	for _, rule := range []string{
		"* text=auto",
		"*.sh text eol=lf",
		"*.go text eol=lf",
		"*.yml text eol=lf",
		"*.yaml text eol=lf",
		"*.ps1 text eol=crlf",
	} {
		if !strings.Contains(string(gitAttrBytes), rule) {
			t.Fatalf(".gitattributes missing required rule %q", rule)
		}
	}

	shBytes, _ := os.ReadFile("../install.sh")
	sh := string(shBytes)
	if strings.Count(sh, `mkdir -p "${INSTALL_DIR}"`) < 3 {
		t.Fatalf("expected install.sh to run mkdir -p \"${INSTALL_DIR}\" across custom, root, and non-root branches")
	}
	if !strings.Contains(sh, `mv -f "${STAGED_BIN}" "${INSTALL_DIR}/${BINARY_NAME}"`) {
		t.Fatalf("expected install.sh to install atomically via temp file + mv -f")
	}

	psBytes, err := os.ReadFile("../install.ps1")
	if err != nil {
		t.Fatalf("read ../install.ps1: %v", err)
	}
	for lineNo, line := range strings.Split(string(psBytes), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "exit" || strings.HasPrefix(trimmed, "exit ") {
			t.Fatalf("install.ps1 line %d uses 'exit' (%q), which kills caller shells under irm | iex", lineNo+1, trimmed)
		}
	}

	for _, wf := range []string{"../.github/workflows/ci.yml", "../.github/workflows/release.yml"} {
		wfRaw, _ := os.ReadFile(wf)
		if !strings.Contains(string(wfRaw), `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24: "true"`) {
			t.Fatalf("expected %s to set FORCE_JAVASCRIPT_ACTIONS_TO_NODE24", wf)
		}
	}
	ciRaw, _ := os.ReadFile("../.github/workflows/ci.yml")
	if !strings.Contains(string(ciRaw), "contents: read") {
		t.Fatalf("expected ci.yml to declare permissions: contents: read")
	}
}


