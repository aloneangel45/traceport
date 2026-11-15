package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestCompressor_RoundTripAndAutoFallback(t *testing.T) {
	comp, err := NewCompressor(WithEncoderDictionary(DefaultSourceDict()))
	if err != nil {
		t.Fatalf("NewCompressor failed: %v", err)
	}
	defer comp.Close()

	cases := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"tiny_go", []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"ok\") }\n")},
		{"binary_payload", []byte{0x00, 0xFF, 0x10, 0x20, 0x7F, 0x80, 0xFE, 0x01}},
		{"repetitive_json", bytes.Repeat([]byte(`{"step":1,"type":"output","content":"agent telemetry line"}`+"\n"), 64)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := comp.Compress(tc.data)
			if err != nil {
				t.Fatalf("Compress failed: %v", err)
			}
			if !IsZstdFrame(encoded) {
				t.Fatalf("expected RFC 8878 zstd magic header, got %x", encoded)
			}

			decoded, err := comp.Decompress(encoded)
			if err != nil {
				t.Fatalf("Decompress failed: %v", err)
			}
			if !bytes.Equal(decoded, tc.data) {
				t.Fatalf("roundtrip mismatch: got %d bytes, want %d", len(decoded), len(tc.data))
			}

			autoDecoded, err := comp.DecompressAuto(encoded)
			if err != nil {
				t.Fatalf("DecompressAuto on zstd frame failed: %v", err)
			}
			if !bytes.Equal(autoDecoded, tc.data) {
				t.Fatalf("DecompressAuto mismatch")
			}
		})
	}

	// Verify transparent fallback on uncompressed legacy/raw payloads.
	rawLegacy := []byte("legacy uncompressed blob without zstd magic bytes")
	out, err := comp.DecompressAuto(rawLegacy)
	if err != nil {
		t.Fatalf("DecompressAuto failed on raw blob: %v", err)
	}
	if !bytes.Equal(out, rawLegacy) {
		t.Fatalf("expected raw legacy bytes unchanged")
	}

	// Strict Decompress must reject non-zstd input and corrupt zstd frames.
	if _, err := comp.Decompress(rawLegacy); err == nil {
		t.Fatalf("expected strict Decompress to reject non-zstd frame")
	}
	corruptZstd := append([]byte{}, ZstdMagic[:]...)
	corruptZstd = append(corruptZstd, 0xFF, 0xFF, 0x00, 0x01)
	if _, err := comp.DecompressAuto(corruptZstd); err == nil {
		t.Fatalf("expected DecompressAuto to fail on corrupt zstd frame with magic header")
	}
}

func TestCompressor_ConcurrentSafety(t *testing.T) {
	comp, err := NewCompressor(WithEncoderDictionary(DefaultSourceDict()))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	defer comp.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, 64)

	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				payload := []byte(fmt.Sprintf("package worker_%d\nfunc Run() int { return %d }\n", id, i))
				enc, err := comp.Compress(payload)
				if err != nil {
					errCh <- err
					return
				}
				dec, err := comp.DecompressAuto(enc)
				if err != nil {
					errCh <- err
					return
				}
				if !bytes.Equal(dec, payload) {
					errCh <- fmt.Errorf("goroutine %d iteration %d mismatch", id, i)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent compressor error: %v", err)
	}
}

func TestDefaultSourceDict_DeterministicAndEffective(t *testing.T) {
	d1 := DefaultSourceDict()
	d2 := DefaultSourceDict()
	if len(d1) == 0 {
		t.Fatalf("expected non-empty DefaultSourceDict")
	}
	if !bytes.Equal(d1, d2) {
		t.Fatalf("DefaultSourceDict must be deterministic")
	}

	rawComp, err := NewCompressor()
	if err != nil {
		t.Fatalf("raw compressor: %v", err)
	}
	defer rawComp.Close()

	dictComp, err := NewCompressor(WithEncoderDictionary(d1))
	if err != nil {
		t.Fatalf("dict compressor: %v", err)
	}
	defer dictComp.Close()

	if dictComp.DictID() != DefaultSourceDictID {
		t.Fatalf("expected DictID %d, got %d", DefaultSourceDictID, dictComp.DictID())
	}

	smallGo := []byte(`package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, errors.New("unauthorized").Error(), http.StatusUnauthorized)
			return
		}
		fmt.Println(context.Background())
		next.ServeHTTP(w, r)
	})
}
`)

	rawEnc, err := rawComp.Compress(smallGo)
	if err != nil {
		t.Fatalf("raw Compress: %v", err)
	}
	dictEnc, err := dictComp.Compress(smallGo)
	if err != nil {
		t.Fatalf("dict Compress: %v", err)
	}
	if len(dictEnc) >= len(rawEnc) {
		t.Fatalf("expected dictionary compression (%dB) to beat raw zstd (%dB) on small Go file", len(dictEnc), len(rawEnc))
	}
}

func TestTrainDictionaryFromDir_SkipsIgnoredAndPreservesHistory(t *testing.T) {
	workDir := t.TempDir()
	storeDir := t.TempDir()

	// Populate multi-file workspace + ignored dirs + binary file + >64KiB file.
	for i := 0; i < 12; i++ {
		src := fmt.Sprintf("package pkg%d\nimport \"fmt\"\nfunc Handle%d(x string) error {\n\tif x == \"\" {\n\t\treturn fmt.Errorf(\"invalid input %%d\", %d)\n\t}\n\treturn nil\n}\n", i, i, i)
		p := filepath.Join(workDir, "pkg", fmt.Sprintf("handler_%d.go", i))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Ignored directories and binary payloads.
	_ = os.MkdirAll(filepath.Join(workDir, "node_modules", "leftpad"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, "node_modules", "leftpad", "index.js"), []byte("module.exports = 1;"), 0644)
	_ = os.MkdirAll(filepath.Join(workDir, ".git"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644)
	_ = os.WriteFile(filepath.Join(workDir, "binary.bin"), []byte{0x00, 0x01, 0x02, 0x00, 0xFF}, 0644)

	largeText := bytes.Repeat([]byte("export const LINE = 'structured source code sample for dictionary cap';\n"), 1500) // >100 KiB
	_ = os.WriteFile(filepath.Join(workDir, "large.ts"), largeText, 0644)

	stats1, err := TrainDictionaryFromDir(workDir, storeDir)
	if err != nil {
		t.Fatalf("TrainDictionaryFromDir #1 failed: %v", err)
	}
	if stats1.SampleCount != 13 { // 12 .go files + 1 large.ts (skips node_modules, .git, binary.bin)
		t.Fatalf("expected 13 sampled source files, got %d", stats1.SampleCount)
	}
	if stats1.DictBytes == 0 {
		t.Fatalf("expected non-empty trained dictionary")
	}

	// Create a snapshot with Dict #1.
	ts, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		t.Fatalf("NewTreeStore: %v", err)
	}
	snap1, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatalf("CreateSnapshot #1: %v", err)
	}
	_ = ts.Close()

	// Modify workspace & train Dict #2; verify blobs compressed with Dict #1 still decode!
	_ = os.WriteFile(filepath.Join(workDir, "new_module.py"), []byte("def compute_metrics(items: list[int]) -> int:\n    return sum(x * 2 for x in items)\n"), 0644)
	stats2, err := TrainDictionaryFromDir(workDir, storeDir)
	if err != nil {
		t.Fatalf("TrainDictionaryFromDir #2 failed: %v", err)
	}
	if stats2.DictID == stats1.DictID {
		t.Fatalf("expected distinct dictionary ID after workspace evolution")
	}

	ts2, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		t.Fatalf("NewTreeStore #2: %v", err)
	}
	defer ts2.Close()

	if _, err := ts2.VerifyTree(snap1); err != nil {
		t.Fatalf("VerifyTree on snapshot from older dictionary failed: %v", err)
	}
}

func TestTreeStore_IncrementalCacheAndDeltaRollback(t *testing.T) {
	workDir := t.TempDir()
	storeDir := t.TempDir()

	// Create .git and .traceport sentinel files to verify they are never touched or deleted.
	_ = os.MkdirAll(filepath.Join(workDir, ".git"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, ".git", "index"), []byte("git-index-sentinel"), 0644)
	_ = os.MkdirAll(filepath.Join(workDir, ".traceport"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, ".traceport", "keep.meta"), []byte("keep"), 0644)

	pastBaseline := time.Now().Add(-10 * time.Second)
	for i := 0; i < 10; i++ {
		p := filepath.Join(workDir, "src", fmt.Sprintf("file_%02d.go", i))
		_ = os.MkdirAll(filepath.Dir(p), 0755)
		_ = os.WriteFile(p, []byte(fmt.Sprintf("package src\nconst V%d = %d\n", i, i)), 0644)
		_ = os.Chtimes(p, pastBaseline, pastBaseline)
	}

	ts, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		t.Fatalf("NewTreeStore: %v", err)
	}
	defer ts.Close()

	hash1, stats1, err := ts.CreateSnapshotWithStats()
	if err != nil {
		t.Fatalf("CreateSnapshot #1: %v", err)
	}
	if stats1.FilesIndexed != 10 || stats1.CacheMisses != 10 || stats1.CacheHits != 0 {
		t.Fatalf("unexpected initial snapshot stats: %+v", stats1)
	}

	// Second snapshot with zero changes: must hit stat cache 100% with 0 disk reads and 0 blob writes.
	hash2, stats2, err := ts.CreateSnapshotWithStats()
	if err != nil {
		t.Fatalf("CreateSnapshot #2: %v", err)
	}
	if hash2 != hash1 {
		t.Fatalf("expected identical content-addressed tree hash: %s vs %s", hash2, hash1)
	}
	if stats2.CacheHits != 10 || stats2.CacheMisses != 0 || stats2.BytesRead != 0 || stats2.BlobsWritten != 0 {
		t.Fatalf("expected zero reads/writes on unchanged snapshot, got: %+v", stats2)
	}

	// Mutate 1 file, delete 1 file, and create 1 post-checkpoint file + nested dir.
	time.Sleep(15 * time.Millisecond)
	modFile := filepath.Join(workDir, "src", "file_03.go")
	_ = os.WriteFile(modFile, []byte("package src\nconst V3 = 999999 // broken edit\n"), 0644)
	delFile := filepath.Join(workDir, "src", "file_07.go")
	_ = os.Remove(delFile)
	extraFile := filepath.Join(workDir, "generated", "temp", "artifact.txt")
	_ = os.MkdirAll(filepath.Dir(extraFile), 0755)
	_ = os.WriteFile(extraFile, []byte("post-checkpoint stray file"), 0644)

	hash3, stats3, err := ts.CreateSnapshotWithStats()
	if err != nil {
		t.Fatalf("CreateSnapshot #3: %v", err)
	}
	if hash3 == hash1 {
		t.Fatalf("expected new tree hash after mutation")
	}
	if stats3.CacheHits != 8 || stats3.CacheMisses != 2 {
		t.Fatalf("expected 8 cache hits and 2 misses (modified + extra), got %+v", stats3)
	}

	// Dry-run plan back to hash1.
	plan, err := ts.PlanRestore(hash1)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	if len(plan.Create) != 1 || len(plan.Modify) != 1 || len(plan.Delete) != 1 || len(plan.Unchanged) != 8 {
		t.Fatalf("unexpected restore plan: %+v", plan)
	}

	// Execute delta rollback to hash1.
	rStats, err := ts.RestoreSnapshotWithStats(hash1)
	if err != nil {
		t.Fatalf("RestoreSnapshotWithStats: %v", err)
	}
	if rStats.FilesRestored != 2 || rStats.FilesDeleted != 1 || rStats.FilesSkipped != 8 {
		t.Fatalf("unexpected restore stats: %+v", rStats)
	}

	// Verify restored file contents, removed stray directory, and untouched .git/.traceport.
	gotMod, err := os.ReadFile(modFile)
	if err != nil || string(gotMod) != "package src\nconst V3 = 3\n" {
		t.Fatalf("file_03.go not restored: %q (err=%v)", string(gotMod), err)
	}
	gotDel, err := os.ReadFile(delFile)
	if err != nil || string(gotDel) != "package src\nconst V7 = 7\n" {
		t.Fatalf("file_07.go not recreated: %q (err=%v)", string(gotDel), err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "generated")); !os.IsNotExist(err) {
		t.Fatalf("expected empty post-checkpoint directory 'generated' to be pruned")
	}
	gitIdx, err := os.ReadFile(filepath.Join(workDir, ".git", "index"))
	if err != nil || string(gitIdx) != "git-index-sentinel" {
		t.Fatalf(".git/index was altered or deleted!")
	}
	tpMeta, err := os.ReadFile(filepath.Join(workDir, ".traceport", "keep.meta"))
	if err != nil || string(tpMeta) != "keep" {
		t.Fatalf(".traceport metadata was altered or deleted!")
	}
}

func TestTreeStore_IntegrityVerificationAndTamperDetection(t *testing.T) {
	workDir := t.TempDir()
	storeDir := t.TempDir()

	_ = os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package main\nfunc main() {}\n"), 0644)

	ts, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	treeHash, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	vRes, err := ts.VerifyTree(treeHash)
	if err != nil {
		t.Fatalf("VerifyTree failed on clean snapshot: %v", err)
	}
	if vRes.FilesVerified != 1 {
		t.Fatalf("expected 1 verified file, got %d", vRes.FilesVerified)
	}

	manifest, err := ts.LoadManifest(treeHash)
	if err != nil {
		t.Fatal(err)
	}
	blobSHA := manifest.Files[0].SHA256

	// Tamper with blob content on disk (valid zstd of DIFFERENT content).
	tampered, err := ts.compressor.Compress([]byte("package main\nfunc main() { panic(\"pwned\") }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ts.blobPath(blobSHA), tampered, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := ts.VerifyTree(treeHash); err == nil {
		t.Fatalf("expected VerifyTree to detect tampered blob content")
	}

	// Remove workspace copy so RestoreSnapshot must read the tampered blob from store.
	_ = os.Remove(filepath.Join(workDir, "app.go"))
	if err := ts.RestoreSnapshot(treeHash); err == nil {
		t.Fatalf("expected RestoreSnapshot to refuse restoring tampered blob")
	}
}

func TestTreeStore_AdversarialPathAndSymlinkProtection(t *testing.T) {
	badPaths := []string{
		"",
		"../escape.go",
		"sub/../../escape.go",
		"/etc/passwd",
		`src\windows\backslash.go`,
		".git/config",
		".traceport/sessions/evil",
		"valid/./dot.go",
		"nul\x00byte.go",
	}
	for _, bp := range badPaths {
		if err := validateRelPath(bp); err == nil {
			t.Errorf("expected validateRelPath(%q) to fail", bp)
		}
	}

	workDir := t.TempDir()
	storeDir := t.TempDir()

	ts, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	// Craft a malicious manifest attempting path traversal and persist it under its hash.
	malicious := TreeManifest{
		Version: 1,
		Files: []FileEntry{
			{
				Path:   "../escaped_file.txt",
				SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				Size:   0,
				Mode:   0644,
			},
		},
	}
	rawJSON, _ := json.Marshal(malicious)
	sum := sha256.Sum256(rawJSON)
	badHash := hex.EncodeToString(sum[:])
	malManifestEnc, err := ts.compressor.Compress(rawJSON)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(ts.manifestPath(badHash), malManifestEnc, 0600)

	if err := ts.RestoreSnapshot(badHash); err == nil {
		t.Fatalf("expected RestoreSnapshot to reject malicious manifest path traversal")
	}

	// Test symlink directory escape prevention and safe unlink-before-restore rewind.
	outsideDir := t.TempDir()
	secretFile := filepath.Join(outsideDir, "secret.txt")
	_ = os.WriteFile(secretFile, []byte("original-outside-secret"), 0644)

	_ = os.MkdirAll(filepath.Join(workDir, "sub"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, "sub", "secret.txt"), []byte("workspace-copy"), 0644)
	goodHash, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	// Replace workDir/sub with a symlink/junction to outsideDir:
	// 1) safeTargetPathLocked MUST refuse direct traversal through symlink directory.
	// 2) RestoreSnapshot MUST safely unlink colliding symlink workDir/sub first, leaving
	//    outsideDir/secret.txt untouched while restoring real workDir/sub/secret.txt!
	_ = os.RemoveAll(filepath.Join(workDir, "sub"))
	if createDirLink(t, outsideDir, filepath.Join(workDir, "sub")) {
		ts.mu.Lock()
		_, travErr := ts.safeTargetPathLocked("sub/secret.txt", true)
		ts.mu.Unlock()
		if travErr == nil {
			t.Fatalf("expected safeTargetPathLocked to block symlink directory traversal")
		}

		if err := ts.RestoreSnapshot(goodHash); err != nil {
			t.Fatalf("RestoreSnapshot failed to unlink colliding directory symlink cleanly: %v", err)
		}
		outsideBytes, _ := os.ReadFile(secretFile)
		if string(outsideBytes) != "original-outside-secret" {
			t.Fatalf("SECURITY FAILURE: RestoreSnapshot overwrote file outside workspace via symlink!")
		}
		linfo, err := os.Lstat(filepath.Join(workDir, "sub"))
		if err != nil || !linfo.IsDir() || IsLinkMode(linfo.Mode()) {
			t.Fatalf("expected workDir/sub restored as real directory, got info=%v err=%v", linfo, err)
		}
		restoredBytes, err := os.ReadFile(filepath.Join(workDir, "sub", "secret.txt"))
		if err != nil || string(restoredBytes) != "workspace-copy" {
			t.Fatalf("expected workDir/sub/secret.txt restored to 'workspace-copy', got %q (err=%v)", string(restoredBytes), err)
		}
	}
}

func TestLegacyGitWorkspaceManager_PreservesGitIndexAndValidatesHashes(t *testing.T) {
	badHashes := []string{
		"",
		"--upload-pack=touch /tmp/pwn",
		"-f",
		"0123456789abcdef",
		"GGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG",
		"0123456789abcdef0123456789abcdef01234567\n--evil",
	}
	for _, bh := range badHashes {
		if IsValidGitHash(bh) {
			t.Errorf("expected IsValidGitHash(%q) == false", bh)
		}
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not installed")
	}

	repoDir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}

	runGit("init")
	runGit("config", "user.email", "test@traceport.local")
	runGit("config", "user.name", "TracePort Test")

	_ = os.WriteFile(filepath.Join(repoDir, "tracked.txt"), []byte("v1\n"), 0644)
	runGit("add", "tracked.txt")
	runGit("commit", "-m", "initial")

	// Stage a specific change in .git/index AND have an unstaged working tree edit + untracked file.
	_ = os.WriteFile(filepath.Join(repoDir, "tracked.txt"), []byte("staged-v2\n"), 0644)
	runGit("add", "tracked.txt")
	_ = os.WriteFile(filepath.Join(repoDir, "tracked.txt"), []byte("unstaged-v3\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "untracked.txt"), []byte("untracked-data\n"), 0644)

	gitIndexPath := filepath.Join(repoDir, ".git", "index")
	indexBefore, err := os.ReadFile(gitIndexPath)
	if err != nil {
		t.Fatalf("read .git/index before snapshot: %v", err)
	}

	wm := NewWorkspaceManager(repoDir)
	if !wm.IsGitRepo() {
		t.Fatalf("expected IsGitRepo() == true")
	}

	snap, err := wm.CreateSnapshot()
	if err != nil {
		t.Fatalf("WorkspaceManager.CreateSnapshot: %v", err)
	}

	indexAfterSnap, err := os.ReadFile(gitIndexPath)
	if err != nil {
		t.Fatalf("read .git/index after snapshot: %v", err)
	}
	if !bytes.Equal(indexBefore, indexAfterSnap) {
		t.Fatalf(".git/index was mutated by CreateSnapshot!")
	}

	// Mutate working file, then restore via legacy git RestoreSnapshot, verifying .git/index stays untouched.
	_ = os.WriteFile(filepath.Join(repoDir, "tracked.txt"), []byte("corrupted\n"), 0644)
	if err := wm.RestoreSnapshot(snap); err != nil {
		t.Fatalf("WorkspaceManager.RestoreSnapshot: %v", err)
	}

	indexAfterRestore, err := os.ReadFile(gitIndexPath)
	if err != nil {
		t.Fatalf("read .git/index after restore: %v", err)
	}
	if !bytes.Equal(indexBefore, indexAfterRestore) {
		t.Fatalf(".git/index was mutated by RestoreSnapshot!")
	}

	restoredTracked, _ := os.ReadFile(filepath.Join(repoDir, "tracked.txt"))
	if string(restoredTracked) != "unstaged-v3\n" {
		t.Fatalf("expected working tree state 'unstaged-v3\\n', got %q", string(restoredTracked))
	}
}

func TestTreeStore_ReadOnlyFilesDirsAndEmptyDirPruning(t *testing.T) {
	workDir := t.TempDir()
	storeDir := t.TempDir()

	// Seed baseline workspace:
	// - locked.cfg (will later be overwritten while read-only 0444)
	// - nested/item.go (parent 'nested' will later be replaced by a read-only file)
	// - target_file.txt (will later be replaced by a directory containing read-only files)
	lockedPath := filepath.Join(workDir, "locked.cfg")
	nestedFile := filepath.Join(workDir, "nested", "item.go")
	targetFile := filepath.Join(workDir, "target_file.txt")

	_ = os.MkdirAll(filepath.Dir(nestedFile), 0755)
	if err := os.WriteFile(lockedPath, []byte("locked=v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedFile, []byte("package nested\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetFile, []byte("clean file v1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ts, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	snap1, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Simulate adversarial post-checkpoint workspace mutations:
	// 1. Mutate locked.cfg and mark it read-only (0444).
	_ = os.WriteFile(lockedPath, []byte("locked=corrupted_v2\n"), 0644)
	_ = os.Chmod(lockedPath, 0444)

	// 2. Create a post-checkpoint untracked read-only file (0444).
	strayRO := filepath.Join(workDir, "ro_sub", "stray_readonly.lock")
	_ = os.MkdirAll(filepath.Dir(strayRO), 0755)
	_ = os.WriteFile(strayRO, []byte("untracked read-only"), 0444)
	_ = os.Chmod(strayRO, 0444)

	// 3. Replace 'nested' directory with a read-only regular file.
	_ = os.RemoveAll(filepath.Join(workDir, "nested"))
	_ = os.WriteFile(filepath.Join(workDir, "nested"), []byte("blocking read-only file"), 0444)
	_ = os.Chmod(filepath.Join(workDir, "nested"), 0444)

	// 4. Replace 'target_file.txt' with a directory containing a read-only file.
	_ = os.Remove(targetFile)
	_ = os.MkdirAll(filepath.Join(targetFile, "inner"), 0755)
	innerRO := filepath.Join(targetFile, "inner", "readonly_child.txt")
	_ = os.WriteFile(innerRO, []byte("blocking dir child"), 0444)
	_ = os.Chmod(innerRO, 0444)

	// 5. Create post-checkpoint empty directories with zero files inside.
	emptyTree := filepath.Join(workDir, "empty_post_dir", "deep_empty")
	_ = os.MkdirAll(emptyTree, 0755)

	if err := ts.RestoreSnapshot(snap1); err != nil {
		t.Fatalf("RestoreSnapshot failed on read-only / conflicting workspace state: %v", err)
	}

	gotLocked, err := os.ReadFile(lockedPath)
	if err != nil || string(gotLocked) != "locked=v1\n" {
		t.Fatalf("locked.cfg not restored: %q (err=%v)", string(gotLocked), err)
	}
	gotNested, err := os.ReadFile(nestedFile)
	if err != nil || string(gotNested) != "package nested\n" {
		t.Fatalf("nested/item.go not restored over read-only blocking file: %q (err=%v)", string(gotNested), err)
	}
	gotTarget, err := os.ReadFile(targetFile)
	if err != nil || string(gotTarget) != "clean file v1\n" {
		t.Fatalf("target_file.txt not restored over read-only conflicting dir: %q (err=%v)", string(gotTarget), err)
	}
	if _, err := os.Stat(strayRO); !os.IsNotExist(err) {
		t.Fatalf("expected post-checkpoint read-only file to be deleted")
	}
	if _, err := os.Stat(filepath.Join(workDir, "empty_post_dir")); !os.IsNotExist(err) {
		t.Fatalf("expected post-checkpoint empty directory tree to be pruned")
	}
}

func TestTreeStore_CrossWorkspaceStatCacheIsolation(t *testing.T) {
	sharedStore := t.TempDir()
	workA := t.TempDir()
	workB := t.TempDir()

	// Both workspaces have "main.go" with identical byte length (24 bytes) and identical mtime!
	contentA := []byte("package alpha // AAAAAA\n")
	contentB := []byte("package bravo // BBBBBB\n")
	if len(contentA) != len(contentB) {
		t.Fatalf("test setup requires equal file sizes")
	}

	fixedTime := time.Date(2025, 11, 14, 10, 0, 0, 0, time.UTC)
	fileA := filepath.Join(workA, "main.go")
	fileB := filepath.Join(workB, "main.go")

	if err := os.WriteFile(fileA, contentA, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fileA, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, contentB, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fileB, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}

	tsA, err := NewTreeStore(workA, sharedStore)
	if err != nil {
		t.Fatal(err)
	}
	snapA, err := tsA.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	_ = tsA.Close()

	tsB, err := NewTreeStore(workB, sharedStore)
	if err != nil {
		t.Fatal(err)
	}
	snapB, statsB, err := tsB.CreateSnapshotWithStats()
	if err != nil {
		t.Fatal(err)
	}
	_ = tsB.Close()

	if snapA == snapB {
		t.Fatalf("cross-workspace stat_cache poisoning: workB inherited workA snapshot hash %s!", snapA)
	}
	if statsB.CacheMisses != 1 || statsB.CacheHits != 0 {
		t.Fatalf("expected cache miss on workB's first snapshot, got %+v", statsB)
	}

	// Re-open workA and verify workA's stat cache was preserved (not wiped by workB).
	tsA2, err := NewTreeStore(workA, sharedStore)
	if err != nil {
		t.Fatal(err)
	}
	defer tsA2.Close()
	_, statsA2, err := tsA2.CreateSnapshotWithStats()
	if err != nil {
		t.Fatal(err)
	}
	if statsA2.CacheHits != 1 || statsA2.CacheMisses != 0 {
		t.Fatalf("expected workA stat cache hit to survive workB snapshot, got %+v", statsA2)
	}
}

func TestTreeStore_ManifestDigestMismatchAndCrossOSWindowsDriveValidation(t *testing.T) {
	crossOSBadPaths := []string{
		"C:/Windows/System32/cmd.exe",
		"c:/users/admin/.ssh/id_rsa",
		"D:relative_drive.go",
		"pkg/file.txt:zone.identifier",
	}
	for _, bp := range crossOSBadPaths {
		if err := validateRelPath(bp); err == nil {
			t.Errorf("expected validateRelPath(%q) to fail across all OSes", bp)
		}
	}

	workDir := t.TempDir()
	storeDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, "a.go"), []byte("package a\n"), 0644)

	ts, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	snap1, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(workDir, "a.go"), []byte("package a\nconst V = 2\n"), 0644)
	snap2, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	_ = ts.Close()

	// Swap snap1's manifest bytes on disk with snap2's manifest bytes so Zstd + blob hashes are valid
	// JSON, ONLY the outer manifest SHA-256 digest mismatches snap1!
	snap2Raw, err := os.ReadFile(filepath.Join(storeDir, "trees", snap2+".manifest.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "trees", snap1+".manifest.zst"), snap2Raw, 0600); err != nil {
		t.Fatal(err)
	}

	tsFresh, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer tsFresh.Close()

	if _, err := tsFresh.LoadManifest(snap1); err == nil {
		t.Fatalf("expected LoadManifest to reject swapped manifest with mismatched SHA-256 digest")
	}
	if err := tsFresh.RestoreSnapshot(snap1); err == nil {
		t.Fatalf("expected RestoreSnapshot to reject swapped manifest with mismatched SHA-256 digest")
	}
}

func createDirLink(t *testing.T, targetDir, linkPath string) bool {
	t.Helper()
	if err := os.Symlink(targetDir, linkPath); err == nil {
		return true
	} else if runtime.GOOS == "windows" {
		if out, jErr := exec.Command("cmd.exe", "/c", "mklink", "/J", linkPath, targetDir).CombinedOutput(); jErr == nil {
			return true
		} else {
			t.Logf("skipping dir link creation on unprivileged Windows: symlink=%v junction=%v (%s)", err, jErr, string(out))
			return false
		}
	} else {
		t.Fatalf("unexpected directory symlink failure: %v", err)
		return false
	}
}

func TestCompressor_UseAfterCloseReturnsError(t *testing.T) {
	comp, err := NewCompressor(WithEncoderDictionary(DefaultSourceDict()))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	sample := []byte("package main\nfunc main() {}\n")
	validFrame, err := comp.Compress(sample)
	if err != nil {
		t.Fatalf("Compress before Close: %v", err)
	}

	if err := comp.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}
	if err := comp.Close(); err != nil {
		t.Fatalf("Close #2 (idempotent): %v", err)
	}

	if _, err := comp.Compress(sample); !errors.Is(err, ErrCompressorClosed) {
		t.Fatalf("expected ErrCompressorClosed from Compress after Close, got: %v", err)
	}
	if _, err := comp.Decompress(validFrame); !errors.Is(err, ErrCompressorClosed) {
		t.Fatalf("expected ErrCompressorClosed from Decompress after Close, got: %v", err)
	}
	if _, err := comp.DecompressAuto(validFrame); !errors.Is(err, ErrCompressorClosed) {
		t.Fatalf("expected ErrCompressorClosed from DecompressAuto(zstd) after Close, got: %v", err)
	}
	if _, err := comp.DecompressAuto([]byte("raw non-zstd")); !errors.Is(err, ErrCompressorClosed) {
		t.Fatalf("expected ErrCompressorClosed from DecompressAuto(raw) after Close, got: %v", err)
	}

	var nilComp *Compressor
	if _, err := nilComp.Compress(sample); !errors.Is(err, ErrCompressorClosed) {
		t.Fatalf("expected ErrCompressorClosed on nil Compressor.Compress, got: %v", err)
	}
}

func TestTreeStore_UncompressedModePreservesRealZstUserFiles(t *testing.T) {
	workDir := t.TempDir()
	storeDir := t.TempDir()

	// Create a genuine Zstandard-compressed user file (.zst) inside the workspace.
	helperComp, err := NewCompressor()
	if err != nil {
		t.Fatal(err)
	}
	defer helperComp.Close()

	innerPlain := []byte("inner payload inside user archive.tar.zst that MUST NOT be decompressed by TreeStore!\n")
	realZstBytes, err := helperComp.Compress(innerPlain)
	if err != nil {
		t.Fatal(err)
	}
	if !IsZstdFrame(realZstBytes) {
		t.Fatalf("expected realZstBytes to begin with RFC 8878 Zstd magic header")
	}

	userZstPath := filepath.Join(workDir, "fixtures", "archive.tar.zst")
	if err := os.MkdirAll(filepath.Dir(userZstPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userZstPath, realZstBytes, 0644); err != nil {
		t.Fatal(err)
	}

	ts, err := NewTreeStore(workDir, storeDir, WithCompressionMode(ModeUncompressed))
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	treeHash, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatalf("CreateSnapshot (ModeUncompressed): %v", err)
	}

	manifest, err := ts.LoadManifest(treeHash)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Size != int64(len(realZstBytes)) {
		t.Fatalf("unexpected manifest entry: %+v", manifest.Files)
	}

	blobBytes, err := ts.ReadBlob(manifest.Files[0].SHA256)
	if err != nil {
		t.Fatalf("ReadBlob on uncompressed .zst user file failed: %v", err)
	}
	if !bytes.Equal(blobBytes, realZstBytes) {
		t.Fatalf("ReadBlob corrupted .zst user file (got %dB, want %dB)", len(blobBytes), len(realZstBytes))
	}

	if vRes, err := ts.VerifyTree(treeHash); err != nil || vRes.FilesVerified != 1 {
		t.Fatalf("VerifyTree failed on uncompressed .zst user file: res=%+v err=%v", vRes, err)
	}

	// Delete workspace file and restore from ModeUncompressed store; verify exact .zst bytes!
	if err := os.Remove(userZstPath); err != nil {
		t.Fatal(err)
	}
	if err := ts.RestoreSnapshot(treeHash); err != nil {
		t.Fatalf("RestoreSnapshot failed: %v", err)
	}
	restored, err := os.ReadFile(userZstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, realZstBytes) {
		t.Fatalf("restored .zst user file mismatch: got %dB, want %dB", len(restored), len(realZstBytes))
	}
}

func TestTreeStore_SymlinkedWorkspaceRootAndDictTraining(t *testing.T) {
	realWork := filepath.Join(t.TempDir(), "real_workspace")
	if err := os.MkdirAll(filepath.Join(realWork, "cmd"), 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		p := filepath.Join(realWork, "cmd", fmt.Sprintf("mod_%d.go", i))
		content := fmt.Sprintf("package cmd\nfunc Step%d() int { return %d }\n", i, i)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	linkedWork := filepath.Join(t.TempDir(), "linked_workspace")
	if !createDirLink(t, realWork, linkedWork) {
		t.Skip("directory symlink/junction unavailable")
	}

	storeDir := t.TempDir()

	// 1. Dictionary training via symlinked workspace root must sample all 6 source files (not 0!).
	dStats, err := TrainDictionaryFromDir(linkedWork, storeDir)
	if err != nil {
		t.Fatalf("TrainDictionaryFromDir on symlinked workspace failed: %v", err)
	}
	if dStats.SampleCount != 6 {
		t.Fatalf("expected 6 sampled files via symlinked workspace root, got %d", dStats.SampleCount)
	}

	// 2. TreeStore CreateSnapshot & RestoreSnapshot via symlinked workspace root must index all 6 files.
	ts, err := NewTreeStore(linkedWork, storeDir)
	if err != nil {
		t.Fatalf("NewTreeStore(linkedWork): %v", err)
	}
	defer ts.Close()

	snap1, stats1, err := ts.CreateSnapshotWithStats()
	if err != nil {
		t.Fatalf("CreateSnapshot via symlinked workspace failed: %v", err)
	}
	if stats1.FilesIndexed != 6 {
		t.Fatalf("expected 6 indexed files via symlinked workspace root, got %d", stats1.FilesIndexed)
	}

	// Mutate 1 file and add 1 stray file through linkedWork, then rewind to snap1.
	targetMod := filepath.Join(linkedWork, "cmd", "mod_0.go")
	strayFile := filepath.Join(linkedWork, "stray.txt")
	_ = os.WriteFile(targetMod, []byte("package cmd\nBROKEN\n"), 0644)
	_ = os.WriteFile(strayFile, []byte("delete me"), 0644)

	rStats, err := ts.RestoreSnapshotWithStats(snap1)
	if err != nil {
		t.Fatalf("RestoreSnapshotWithStats via symlinked workspace failed: %v", err)
	}
	if rStats.FilesRestored != 1 || rStats.FilesDeleted != 1 || rStats.FilesSkipped != 5 {
		t.Fatalf("unexpected restore stats via symlinked workspace: %+v", rStats)
	}
	got, err := os.ReadFile(targetMod)
	if err != nil || string(got) != "package cmd\nfunc Step0() int { return 0 }\n" {
		t.Fatalf("mod_0.go not restored via symlinked workspace: %q (err=%v)", string(got), err)
	}
}

func TestTreeStore_PreservesPreExistingSymlinksAndUnlinksCollidingSymlinks(t *testing.T) {
	workDir := t.TempDir()
	storeDir := t.TempDir()
	externalSafeDir := t.TempDir()
	externalAttackDir := t.TempDir()

	_ = os.WriteFile(filepath.Join(externalSafeDir, "keep.txt"), []byte("external-safe"), 0644)
	_ = os.WriteFile(filepath.Join(externalAttackDir, "api.go"), []byte("external-do-not-touch"), 0644)

	// Seed tracked regular files.
	_ = os.MkdirAll(filepath.Join(workDir, "src"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, "src", "main.go"), []byte("package main\n"), 0644)
	_ = os.MkdirAll(filepath.Join(workDir, "pkg"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, "pkg", "api.go"), []byte("package pkg\nfunc OK() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# Clean Readme\n"), 0644)

	// Seed pre-existing non-colliding workspace symlinks (at root and inside tracked dir src/).
	rootLink := filepath.Join(workDir, "vendor_link")
	srcLink := filepath.Join(workDir, "src", "docs_link")
	if !createDirLink(t, externalSafeDir, rootLink) || !createDirLink(t, externalSafeDir, srcLink) {
		t.Skip("directory symlink/junction unavailable")
	}

	ts, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	snap, err := ts.CreateSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate post-checkpoint agent havoc:
	// 1. Replace tracked directory pkg/ with a symlink pkg -> externalAttackDir.
	_ = os.RemoveAll(filepath.Join(workDir, "pkg"))
	if !createDirLink(t, externalAttackDir, filepath.Join(workDir, "pkg")) {
		t.Fatal("failed creating colliding pkg symlink")
	}

	// 2. Replace tracked file README.md with a symlink README.md -> externalAttackDir.
	_ = os.Remove(filepath.Join(workDir, "README.md"))
	if !createDirLink(t, externalAttackDir, filepath.Join(workDir, "README.md")) {
		t.Fatal("failed creating colliding README.md symlink")
	}

	// 3. Create a brand-new post-checkpoint directory containing a symlink.
	newAgentDir := filepath.Join(workDir, "new_agent_dir")
	_ = os.MkdirAll(newAgentDir, 0755)
	if !createDirLink(t, externalAttackDir, filepath.Join(newAgentDir, "nested_link")) {
		t.Fatal("failed creating symlink inside new_agent_dir")
	}

	plan, err := ts.PlanRestore(snap)
	if err != nil {
		t.Fatalf("PlanRestore failed: %v", err)
	}
	for _, delPath := range plan.Delete {
		if delPath == "vendor_link" || delPath == "src/docs_link" {
			t.Fatalf("PlanRestore wrongly scheduled pre-existing symlink %q for deletion!", delPath)
		}
	}

	if _, err := ts.RestoreSnapshotWithStats(snap); err != nil {
		t.Fatalf("RestoreSnapshotWithStats failed: %v", err)
	}

	// Verify pre-existing non-colliding symlinks were preserved!
	if info, err := os.Lstat(rootLink); err != nil || !IsLinkMode(info.Mode()) {
		t.Fatalf("expected pre-existing root symlink vendor_link to be preserved, got info=%v err=%v", info, err)
	}
	if info, err := os.Lstat(srcLink); err != nil || !IsLinkMode(info.Mode()) {
		t.Fatalf("expected pre-existing src/docs_link symlink to be preserved, got info=%v err=%v", info, err)
	}

	// Verify colliding pkg symlink was unlinked and real pkg/api.go restored without touching externalAttackDir!
	pkgInfo, err := os.Lstat(filepath.Join(workDir, "pkg"))
	if err != nil || !pkgInfo.IsDir() || IsLinkMode(pkgInfo.Mode()) {
		t.Fatalf("expected pkg restored as real directory, got info=%v err=%v", pkgInfo, err)
	}
	apiBytes, err := os.ReadFile(filepath.Join(workDir, "pkg", "api.go"))
	if err != nil || string(apiBytes) != "package pkg\nfunc OK() {}\n" {
		t.Fatalf("expected pkg/api.go restored, got %q (err=%v)", string(apiBytes), err)
	}
	extApiBytes, _ := os.ReadFile(filepath.Join(externalAttackDir, "api.go"))
	if string(extApiBytes) != "external-do-not-touch" {
		t.Fatalf("SECURITY FAILURE: external target mutated during rewind!")
	}

	// Verify colliding README.md symlink was replaced with real regular file!
	readmeInfo, err := os.Lstat(filepath.Join(workDir, "README.md"))
	if err != nil || !readmeInfo.Mode().IsRegular() {
		t.Fatalf("expected README.md restored as regular file, got info=%v err=%v", readmeInfo, err)
	}

	// Verify symlink inside newly created directory + new_agent_dir itself were removed!
	if _, err := os.Lstat(newAgentDir); !os.IsNotExist(err) {
		t.Fatalf("expected newly created directory new_agent_dir and its symlink to be pruned, err=%v", err)
	}
}

func TestTreeStore_StatCacheRacyCleanAndPostRestoreOverwrite(t *testing.T) {
	workDir := t.TempDir()
	storeDir := t.TempDir()

	target := filepath.Join(workDir, "config.env")
	v1 := []byte("TOKEN=alpha_111\n") // 16 bytes
	v2 := []byte("TOKEN=bravo_222\n") // 16 bytes (identical size!)
	v3 := []byte("TOKEN=gamma_333\n") // 16 bytes (identical size!)

	if err := os.WriteFile(target, v1, 0644); err != nil {
		t.Fatal(err)
	}
	info1, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	ts, err := NewTreeStore(workDir, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	snap1, _, err := ts.CreateSnapshotWithStats()
	if err != nil {
		t.Fatal(err)
	}

	// 1. Immediate (<40ms) same-size overwrite with identical mtime inside the 2s racy-clean window!
	if err := os.WriteFile(target, v2, 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(target, info1.ModTime(), info1.ModTime())

	snap2, stats2, err := ts.CreateSnapshotWithStats()
	if err != nil {
		t.Fatal(err)
	}
	if snap2 == snap1 {
		t.Fatalf("racy-clean bug: same-size sub-tick overwrite recorded stale snapshot hash %s!", snap2)
	}
	if stats2.CacheMisses != 1 {
		t.Fatalf("expected racy-clean window to bypass statCache hit, got %+v", stats2)
	}

	// 2. Restore snap1, then immediately perform a same-size edit with identical mtime!
	if err := ts.RestoreSnapshot(snap1); err != nil {
		t.Fatal(err)
	}
	restoredInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, v3, 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(target, restoredInfo.ModTime(), restoredInfo.ModTime())

	snap3, stats3, err := ts.CreateSnapshotWithStats()
	if err != nil {
		t.Fatal(err)
	}
	if snap3 == snap1 || snap3 == snap2 {
		t.Fatalf("post-restore racy-clean bug: immediate same-size edit produced stale hash %s", snap3)
	}
	if stats3.CacheMisses != 1 {
		t.Fatalf("expected post-restore racy-clean edit to re-hash file, got %+v", stats3)
	}
}
