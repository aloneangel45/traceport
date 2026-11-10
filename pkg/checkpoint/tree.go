package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// TreeCompressionMode selects how blobs and manifests are stored by TreeStore.
type TreeCompressionMode int

const (
	// ModeZstdDict compresses blobs and manifests with Zstandard + active dictionary (default).
	ModeZstdDict TreeCompressionMode = iota
	// ModeZstdRaw compresses blobs and manifests with raw Zstandard (no dictionary).
	ModeZstdRaw
	// ModeUncompressed stores raw uncompressed bytes (used for baseline benchmarking & testing).
	ModeUncompressed
)

// FileEntry describes a single tracked regular file in a content-addressed workspace manifest.
type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

// TreeManifest represents an immutable workspace snapshot state.
type TreeManifest struct {
	Version int         `json:"version"`
	Files   []FileEntry `json:"files"`
}

type statCacheEntry struct {
	ModTimeUnixNano int64  `json:"mtime_ns"`
	Size            int64  `json:"size"`
	Mode            uint32 `json:"mode"`
	SHA256          string `json:"sha256"`
}

// SnapshotStats tracks incremental I/O and compression activity for a CreateSnapshot call.
type SnapshotStats struct {
	FilesIndexed int   `json:"files_indexed"`
	CacheHits    int   `json:"cache_hits"`
	CacheMisses  int   `json:"cache_misses"`
	BytesRead    int64 `json:"bytes_read"`
	BlobsWritten int   `json:"blobs_written"`
}

// RestorePlan previews workspace mutations that RestoreSnapshot will perform.
type RestorePlan struct {
	TreeHash  string   `json:"tree_hash"`
	Create    []string `json:"create"`
	Modify    []string `json:"modify"`
	Delete    []string `json:"delete"`
	Unchanged []string `json:"unchanged"`
}

// RestoreStats reports delta rollback work performed by RestoreSnapshot.
type RestoreStats struct {
	TreeHash      string `json:"tree_hash"`
	FilesRestored int    `json:"files_restored"`
	FilesDeleted  int    `json:"files_deleted"`
	FilesSkipped  int    `json:"files_skipped"`
	BytesWritten  int64  `json:"bytes_written"`
}

// TreeVerifyResult summarizes cryptographic integrity verification of a tree manifest and its blobs.
type TreeVerifyResult struct {
	TreeHash      string `json:"tree_hash"`
	FilesVerified int    `json:"files_verified"`
	BytesVerified int64  `json:"bytes_verified"`
}

// TreeOption configures a TreeStore instance.
type TreeOption func(*treeStoreConfig)

type treeStoreConfig struct {
	mode       TreeCompressionMode
	customDict []byte
}

// WithCompressionMode configures the compression mode used by TreeStore.
func WithCompressionMode(mode TreeCompressionMode) TreeOption {
	return func(cfg *treeStoreConfig) {
		cfg.mode = mode
	}
}

// WithCustomDictionary overrides the active dictionary used in ModeZstdDict.
func WithCustomDictionary(dictBytes []byte) TreeOption {
	return func(cfg *treeStoreConfig) {
		if len(dictBytes) > 0 {
			cp := make([]byte, len(dictBytes))
			copy(cp, dictBytes)
			cfg.customDict = cp
		}
	}
}

// TreeStore manages high-frequency content-addressed Zstandard shadow-tree snapshots.
type TreeStore struct {
	mu            sync.Mutex
	workspaceDir  string
	realWorkspace string
	storeDir      string
	mode          TreeCompressionMode
	compressor    *Compressor
	statCache     map[string]statCacheEntry
	knownBlobs    map[string]bool
	manifestCache map[string]*TreeManifest
}

// NewTreeStore initializes a content-addressed shadow-tree snapshotter for workspaceDir.
func NewTreeStore(workspaceDir, storeDir string, opts ...TreeOption) (*TreeStore, error) {
	cfg := treeStoreConfig{
		mode: ModeZstdDict,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	absWork, err := filepath.Abs(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace path: %w", err)
	}
	realWork := absWork
	if eval, err := filepath.EvalSymlinks(absWork); err == nil {
		realWork = eval
	}

	absStore, err := filepath.Abs(storeDir)
	if err != nil {
		return nil, fmt.Errorf("resolve store path: %w", err)
	}

	for _, sub := range []string{"blobs", "trees", "dicts"} {
		if err := os.MkdirAll(filepath.Join(absStore, sub), 0700); err != nil {
			return nil, fmt.Errorf("initialize store directory %s: %w", sub, err)
		}
	}

	activeDict, allDicts, err := LoadStoreDictionaries(absStore)
	if err != nil {
		return nil, fmt.Errorf("load store dictionaries: %w", err)
	}
	if len(cfg.customDict) > 0 {
		activeDict = cfg.customDict
		allDicts = append(allDicts, cfg.customDict)
	}

	compOpts := []CompressorOption{
		WithLevel(zstd.SpeedFastest),
		WithDecoderDictionaries(allDicts...),
	}
	if cfg.mode == ModeZstdDict && len(activeDict) > 0 {
		compOpts = append(compOpts, WithEncoderDictionary(activeDict))
	}

	comp, err := NewCompressor(compOpts...)
	if err != nil {
		return nil, err
	}

	ts := &TreeStore{
		workspaceDir:  absWork,
		realWorkspace: realWork,
		storeDir:      absStore,
		mode:          cfg.mode,
		compressor:    comp,
		statCache:     make(map[string]statCacheEntry),
		knownBlobs:    make(map[string]bool),
		manifestCache: make(map[string]*TreeManifest),
	}
	ts.loadStatCacheLocked()
	return ts, nil
}

// ReloadDictionaries refreshes encoder and decoder dictionaries from <storeDir>/dicts/.
func (ts *TreeStore) ReloadDictionaries() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	activeDict, allDicts, err := LoadStoreDictionaries(ts.storeDir)
	if err != nil {
		return err
	}
	if err := ts.compressor.RegisterDecoderDicts(allDicts...); err != nil {
		return err
	}
	if ts.mode == ModeZstdDict && len(activeDict) > 0 {
		return ts.compressor.SetEncoderDict(activeDict)
	}
	return nil
}

// Close releases underlying Zstandard codec resources.
func (ts *TreeStore) Close() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.compressor != nil {
		return ts.compressor.Close()
	}
	return nil
}

func (ts *TreeStore) statCachePath() string {
	return filepath.Join(ts.storeDir, "trees", "stat_cache.json")
}

func (ts *TreeStore) loadStatCacheLocked() {
	data, err := os.ReadFile(ts.statCachePath())
	if err != nil || len(data) == 0 {
		return
	}
	var loaded map[string]statCacheEntry
	if err := json.Unmarshal(data, &loaded); err == nil && loaded != nil {
		ts.statCache = loaded
	}
}

func (ts *TreeStore) saveStatCacheLocked() {
	data, err := json.Marshal(ts.statCache)
	if err != nil {
		return
	}
	_ = writeAtomicFile(ts.statCachePath(), data, 0600)
}

func (ts *TreeStore) blobPath(sha string) string {
	return filepath.Join(ts.storeDir, "blobs", sha[:2], sha[2:]+".zst")
}

func (ts *TreeStore) manifestPath(treeHash string) string {
	return filepath.Join(ts.storeDir, "trees", treeHash+".manifest.zst")
}

func (ts *TreeStore) hasBlobLocked(sha string) bool {
	if ts.knownBlobs[sha] {
		return true
	}
	if info, err := os.Stat(ts.blobPath(sha)); err == nil && !info.IsDir() {
		ts.knownBlobs[sha] = true
		return true
	}
	return false
}

// HasTree reports whether a manifest for treeHash exists in <storeDir>/trees/.
func (ts *TreeStore) HasTree(treeHash string) bool {
	if !isValidSHA256Hex(treeHash) {
		return false
	}
	ts.mu.Lock()
	if _, ok := ts.manifestCache[treeHash]; ok {
		ts.mu.Unlock()
		return true
	}
	ts.mu.Unlock()

	info, err := os.Stat(ts.manifestPath(treeHash))
	return err == nil && !info.IsDir()
}

// CreateSnapshot captures a content-addressed workspace shadow-tree snapshot without touching git.
func (ts *TreeStore) CreateSnapshot() (string, error) {
	hash, _, err := ts.CreateSnapshotWithStats()
	return hash, err
}

// CreateSnapshotWithStats captures a workspace snapshot and returns detailed cache/IO counters.
func (ts *TreeStore) CreateSnapshotWithStats() (string, SnapshotStats, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	var stats SnapshotStats
	files, err := ts.scanWorkspaceLocked(&stats)
	if err != nil {
		return "", stats, err
	}
	stats.FilesIndexed = len(files)

	manifest := &TreeManifest{
		Version: 1,
		Files:   files,
	}

	canonicalBytes, err := json.Marshal(manifest)
	if err != nil {
		return "", stats, fmt.Errorf("marshal manifest: %w", err)
	}

	sum := sha256.Sum256(canonicalBytes)
	treeHash := hex.EncodeToString(sum[:])

	manifestFile := ts.manifestPath(treeHash)
	if _, err := os.Stat(manifestFile); os.IsNotExist(err) {
		encoded := ts.encodeBytesLocked(canonicalBytes)
		if err := writeAtomicFile(manifestFile, encoded, 0600); err != nil {
			return "", stats, fmt.Errorf("write manifest %s: %w", treeHash, err)
		}
	}

	// Copy manifest into in-memory cache for microsecond seek.
	cachedCopy := &TreeManifest{
		Version: manifest.Version,
		Files:   make([]FileEntry, len(manifest.Files)),
	}
	copy(cachedCopy.Files, manifest.Files)
	ts.manifestCache[treeHash] = cachedCopy

	if stats.CacheMisses > 0 || len(ts.statCache) != len(files) {
		// Prune deleted paths from statCache.
		seen := make(map[string]struct{}, len(files))
		for _, f := range files {
			seen[f.Path] = struct{}{}
		}
		for k := range ts.statCache {
			if _, ok := seen[k]; !ok {
				delete(ts.statCache, k)
			}
		}
		ts.saveStatCacheLocked()
	}

	return treeHash, stats, nil
}

func (ts *TreeStore) encodeBytesLocked(raw []byte) []byte {
	if ts.mode == ModeUncompressed {
		cp := make([]byte, len(raw))
		copy(cp, raw)
		return cp
	}
	return ts.compressor.Compress(raw)
}

func (ts *TreeStore) scanWorkspaceLocked(stats *SnapshotStats) ([]FileEntry, error) {
	storeInsideWork := isSubpath(ts.workspaceDir, ts.storeDir) && !sameFilePath(ts.workspaceDir, ts.storeDir)
	sameWorkAndStore := sameFilePath(ts.workspaceDir, ts.storeDir)

	var entries []FileEntry

	err := filepath.WalkDir(ts.workspaceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if path == ts.workspaceDir {
			return nil
		}

		// Exclude store directory if located inside workspace.
		if storeInsideWork && (sameFilePath(path, ts.storeDir) || isSubpath(ts.storeDir, path)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		name := d.Name()
		if name == ".git" || name == ".traceport" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if sameWorkAndStore {
			if d.IsDir() && (name == "blobs" || name == "trees" || name == "dicts" || name == "sessions") {
				return filepath.SkipDir
			}
			if !d.IsDir() && isInternalStoreArtifact(name) {
				return nil
			}
		}

		// Never follow symlinks: prevents workspace escape exfiltration.
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		relOS, err := filepath.Rel(ts.workspaceDir, path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relOS)
		if err := validateRelPath(rel); err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		mtimeNS := info.ModTime().UnixNano()
		size := info.Size()
		normMode := normalizeFileMode(info.Mode())

		if cached, ok := ts.statCache[rel]; ok &&
			cached.ModTimeUnixNano == mtimeNS &&
			cached.Size == size &&
			cached.Mode == normMode &&
			ts.hasBlobLocked(cached.SHA256) {
			stats.CacheHits++
			entries = append(entries, FileEntry{
				Path:   rel,
				SHA256: cached.SHA256,
				Size:   size,
				Mode:   normMode,
			})
			return nil
		}

		stats.CacheMisses++
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("read workspace file %s: %w", rel, err)
		}
		stats.BytesRead += int64(len(data))

		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])

		if !ts.hasBlobLocked(sha) {
			blobFile := ts.blobPath(sha)
			if err := os.MkdirAll(filepath.Dir(blobFile), 0700); err != nil {
				return fmt.Errorf("create blob fanout dir: %w", err)
			}
			compressed := ts.encodeBytesLocked(data)
			if err := writeAtomicFile(blobFile, compressed, 0600); err != nil {
				return fmt.Errorf("write blob %s: %w", sha, err)
			}
			ts.knownBlobs[sha] = true
			stats.BlobsWritten++
		}

		realSize := int64(len(data))
		ts.statCache[rel] = statCacheEntry{
			ModTimeUnixNano: mtimeNS,
			Size:            realSize,
			Mode:            normMode,
			SHA256:          sha,
		}
		entries = append(entries, FileEntry{
			Path:   rel,
			SHA256: sha,
			Size:   realSize,
			Mode:   normMode,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	if entries == nil {
		entries = []FileEntry{}
	}
	return entries, nil
}

// LoadManifest reads and decodes a content-addressed TreeManifest in microseconds.
func (ts *TreeStore) LoadManifest(treeHash string) (*TreeManifest, error) {
	if !isValidSHA256Hex(treeHash) {
		return nil, fmt.Errorf("invalid tree hash %q", treeHash)
	}

	ts.mu.Lock()
	if cached, ok := ts.manifestCache[treeHash]; ok {
		cp := &TreeManifest{
			Version: cached.Version,
			Files:   make([]FileEntry, len(cached.Files)),
		}
		copy(cp.Files, cached.Files)
		ts.mu.Unlock()
		return cp, nil
	}
	ts.mu.Unlock()

	raw, err := os.ReadFile(ts.manifestPath(treeHash))
	if err != nil {
		return nil, fmt.Errorf("manifest %s not found: %w", treeHash, err)
	}

	decoded, err := ts.compressor.DecompressAuto(raw)
	if err != nil {
		return nil, fmt.Errorf("decompress manifest %s: %w", treeHash, err)
	}

	var manifest TreeManifest
	if err := json.Unmarshal(decoded, &manifest); err != nil {
		return nil, fmt.Errorf("unmarshal manifest %s: %w", treeHash, err)
	}

	for _, f := range manifest.Files {
		if err := validateRelPath(f.Path); err != nil {
			return nil, fmt.Errorf("manifest %s contains unsafe path %q: %w", treeHash, f.Path, err)
		}
		if !isValidSHA256Hex(f.SHA256) {
			return nil, fmt.Errorf("manifest %s contains invalid blob hash %q", treeHash, f.SHA256)
		}
	}

	ts.mu.Lock()
	cached := &TreeManifest{
		Version: manifest.Version,
		Files:   make([]FileEntry, len(manifest.Files)),
	}
	copy(cached.Files, manifest.Files)
	ts.manifestCache[treeHash] = cached
	ts.mu.Unlock()

	return &manifest, nil
}

// ReadBlob reads, decompresses, and cryptographically verifies a single content-addressed blob.
func (ts *TreeStore) ReadBlob(sha string) ([]byte, error) {
	if !isValidSHA256Hex(sha) {
		return nil, fmt.Errorf("invalid blob sha256 %q", sha)
	}

	compressed, err := os.ReadFile(ts.blobPath(sha))
	if err != nil {
		return nil, fmt.Errorf("blob %s missing: %w", sha, err)
	}

	plain, err := ts.compressor.DecompressAuto(compressed)
	if err != nil {
		return nil, fmt.Errorf("blob %s decompress failed: %w", sha, err)
	}

	sum := sha256.Sum256(plain)
	actual := hex.EncodeToString(sum[:])
	if actual != sha {
		return nil, fmt.Errorf("blob %s integrity mismatch: got %s", sha, actual)
	}
	return plain, nil
}

// PlanRestore computes a dry-run preview of workspace changes required to reach treeHash.
func (ts *TreeStore) PlanRestore(treeHash string) (*RestorePlan, error) {
	manifest, err := ts.LoadManifest(treeHash)
	if err != nil {
		return nil, err
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	diskFiles, err := ts.listCurrentWorkspacePathsLocked()
	if err != nil {
		return nil, err
	}

	plan := &RestorePlan{
		TreeHash:  treeHash,
		Create:    []string{},
		Modify:    []string{},
		Delete:    []string{},
		Unchanged: []string{},
	}

	targetSet := make(map[string]FileEntry, len(manifest.Files))
	for _, entry := range manifest.Files {
		targetSet[entry.Path] = entry
		info, exists := diskFiles[entry.Path]
		if !exists {
			plan.Create = append(plan.Create, entry.Path)
			continue
		}
		if ts.fileMatchesEntryLocked(entry, info) {
			plan.Unchanged = append(plan.Unchanged, entry.Path)
		} else {
			plan.Modify = append(plan.Modify, entry.Path)
		}
	}

	for rel := range diskFiles {
		if _, keep := targetSet[rel]; !keep {
			plan.Delete = append(plan.Delete, rel)
		}
	}
	sort.Strings(plan.Delete)
	return plan, nil
}

// RestoreSnapshot performs fast delta rollback of workspaceDir to the exact state in treeHash.
func (ts *TreeStore) RestoreSnapshot(treeHash string) error {
	_, err := ts.RestoreSnapshotWithStats(treeHash)
	return err
}

// RestoreSnapshotWithStats performs delta rollback and returns metrics on restored/deleted/skipped files.
func (ts *TreeStore) RestoreSnapshotWithStats(treeHash string) (*RestoreStats, error) {
	manifest, err := ts.LoadManifest(treeHash)
	if err != nil {
		return nil, err
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	stats := &RestoreStats{TreeHash: treeHash}

	diskFiles, err := ts.listCurrentWorkspacePathsLocked()
	if err != nil {
		return nil, err
	}

	targetMap := make(map[string]FileEntry, len(manifest.Files))
	for _, e := range manifest.Files {
		if _, err := ts.safeTargetPathLocked(e.Path, false); err != nil {
			return nil, err
		}
		targetMap[e.Path] = e
	}

	// 1. Delete untracked files created after target checkpoint (preserving .git and .traceport).
	var toDelete []string
	for rel := range diskFiles {
		if _, ok := targetMap[rel]; !ok {
			toDelete = append(toDelete, rel)
		}
	}
	sort.Strings(toDelete)

	for _, rel := range toDelete {
		targetPath, err := ts.safeTargetPathLocked(rel, false)
		if err != nil {
			return nil, err
		}
		if linfo, lerr := os.Lstat(targetPath); lerr == nil && linfo.Mode()&os.ModeSymlink == 0 {
			_ = os.Chmod(targetPath, 0600)
		}
		if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove post-checkpoint file %s: %w", rel, err)
		}
		delete(ts.statCache, rel)
		stats.FilesDeleted++
		ts.pruneEmptyParentsLocked(filepath.Dir(targetPath))
	}

	// 2. Delta-restore missing or modified files; skip unchanged files in microseconds.
	for _, entry := range manifest.Files {
		if info, exists := diskFiles[entry.Path]; exists && ts.fileMatchesEntryLocked(entry, info) {
			stats.FilesSkipped++
			continue
		}

		plain, err := ts.readBlobLocked(entry.SHA256)
		if err != nil {
			return nil, fmt.Errorf("restore %s: %w", entry.Path, err)
		}

		targetPath, err := ts.safeTargetPathLocked(entry.Path, true)
		if err != nil {
			return nil, err
		}

		// If target is currently a directory or symlink, remove it before atomic replacement.
		if linfo, err := os.Lstat(targetPath); err == nil {
			if linfo.Mode()&os.ModeSymlink != 0 {
				if err := os.Remove(targetPath); err != nil {
					return nil, fmt.Errorf("remove symlink %s: %w", entry.Path, err)
				}
			} else if linfo.IsDir() {
				if err := os.RemoveAll(targetPath); err != nil {
					return nil, fmt.Errorf("remove conflicting dir %s: %w", entry.Path, err)
				}
			}
		}

		perm := os.FileMode(entry.Mode) & 0777
		if perm == 0 {
			perm = 0644
		}
		if err := writeAtomicFile(targetPath, plain, perm); err != nil {
			return nil, fmt.Errorf("restore file %s: %w", entry.Path, err)
		}

		if info, err := os.Lstat(targetPath); err == nil {
			ts.statCache[entry.Path] = statCacheEntry{
				ModTimeUnixNano: info.ModTime().UnixNano(),
				Size:            info.Size(),
				Mode:            normalizeFileMode(info.Mode()),
				SHA256:          entry.SHA256,
			}
		}

		stats.FilesRestored++
		stats.BytesWritten += int64(len(plain))
	}

	ts.saveStatCacheLocked()
	return stats, nil
}

// VerifyTree cryptographically verifies the manifest digest and every referenced zstd blob.
func (ts *TreeStore) VerifyTree(treeHash string) (*TreeVerifyResult, error) {
	if !isValidSHA256Hex(treeHash) {
		return nil, fmt.Errorf("invalid tree hash %q", treeHash)
	}

	rawManifest, err := os.ReadFile(ts.manifestPath(treeHash))
	if err != nil {
		return nil, fmt.Errorf("manifest %s missing: %w", treeHash, err)
	}

	decodedManifest, err := ts.compressor.DecompressAuto(rawManifest)
	if err != nil {
		return nil, fmt.Errorf("manifest %s corrupt zstd stream: %w", treeHash, err)
	}

	sum := sha256.Sum256(decodedManifest)
	actualTreeHash := hex.EncodeToString(sum[:])
	if actualTreeHash != treeHash {
		return nil, fmt.Errorf("manifest %s digest mismatch: got %s", treeHash, actualTreeHash)
	}

	var manifest TreeManifest
	if err := json.Unmarshal(decodedManifest, &manifest); err != nil {
		return nil, fmt.Errorf("manifest %s invalid JSON: %w", treeHash, err)
	}

	res := &TreeVerifyResult{TreeHash: treeHash}
	verifiedBlobs := make(map[string]int64, len(manifest.Files))

	for _, entry := range manifest.Files {
		if err := validateRelPath(entry.Path); err != nil {
			return nil, fmt.Errorf("manifest %s unsafe path %q: %w", treeHash, entry.Path, err)
		}
		if !isValidSHA256Hex(entry.SHA256) {
			return nil, fmt.Errorf("manifest %s invalid blob hash %q", treeHash, entry.SHA256)
		}

		blobSize, alreadyChecked := verifiedBlobs[entry.SHA256]
		if !alreadyChecked {
			plain, err := ts.ReadBlob(entry.SHA256)
			if err != nil {
				return nil, fmt.Errorf("manifest %s file %s: %w", treeHash, entry.Path, err)
			}
			blobSize = int64(len(plain))
			verifiedBlobs[entry.SHA256] = blobSize
		}

		if blobSize != entry.Size {
			return nil, fmt.Errorf("manifest %s file %s size mismatch: blob=%d manifest=%d", treeHash, entry.Path, blobSize, entry.Size)
		}
		res.FilesVerified++
		res.BytesVerified += blobSize
	}

	return res, nil
}

func (ts *TreeStore) readBlobLocked(sha string) ([]byte, error) {
	compressed, err := os.ReadFile(ts.blobPath(sha))
	if err != nil {
		return nil, fmt.Errorf("blob %s missing: %w", sha, err)
	}
	plain, err := ts.compressor.DecompressAuto(compressed)
	if err != nil {
		return nil, fmt.Errorf("blob %s decompress failed: %w", sha, err)
	}
	sum := sha256.Sum256(plain)
	if hex.EncodeToString(sum[:]) != sha {
		return nil, fmt.Errorf("blob %s sha256 integrity mismatch", sha)
	}
	return plain, nil
}

func (ts *TreeStore) fileMatchesEntryLocked(entry FileEntry, info fs.FileInfo) bool {
	if !info.Mode().IsRegular() || info.Size() != entry.Size {
		return false
	}
	mtimeNS := info.ModTime().UnixNano()
	normMode := normalizeFileMode(info.Mode())

	if cached, ok := ts.statCache[entry.Path]; ok &&
		cached.ModTimeUnixNano == mtimeNS &&
		cached.Size == info.Size() &&
		cached.Mode == normMode &&
		cached.SHA256 == entry.SHA256 {
		return true
	}

	fullPath := filepath.Join(ts.workspaceDir, filepath.FromSlash(entry.Path))
	f, err := os.Open(fullPath)
	if err != nil {
		return false
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	_ = f.Close()
	if copyErr != nil {
		return false
	}
	diskSHA := hex.EncodeToString(h.Sum(nil))
	if diskSHA != entry.SHA256 {
		return false
	}

	if runtime.GOOS != "windows" && normMode != entry.Mode {
		_ = os.Chmod(fullPath, os.FileMode(entry.Mode)&0777)
	}

	ts.statCache[entry.Path] = statCacheEntry{
		ModTimeUnixNano: mtimeNS,
		Size:            info.Size(),
		Mode:            normMode,
		SHA256:          diskSHA,
	}
	return true
}

func (ts *TreeStore) listCurrentWorkspacePathsLocked() (map[string]fs.FileInfo, error) {
	storeInsideWork := isSubpath(ts.workspaceDir, ts.storeDir) && !sameFilePath(ts.workspaceDir, ts.storeDir)
	sameWorkAndStore := sameFilePath(ts.workspaceDir, ts.storeDir)
	result := make(map[string]fs.FileInfo)

	err := filepath.WalkDir(ts.workspaceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if path == ts.workspaceDir {
			return nil
		}
		if storeInsideWork && (sameFilePath(path, ts.storeDir) || isSubpath(ts.storeDir, path)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		name := d.Name()
		if name == ".git" || name == ".traceport" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if sameWorkAndStore {
			if d.IsDir() && (name == "blobs" || name == "trees" || name == "dicts" || name == "sessions") {
				return filepath.SkipDir
			}
			if !d.IsDir() && isInternalStoreArtifact(name) {
				return nil
			}
		}

		if d.Type()&os.ModeSymlink != 0 {
			// Record untracked symlinks as removable files if created post-checkpoint,
			// without following them into external directories.
			relOS, err := filepath.Rel(ts.workspaceDir, path)
			if err == nil {
				rel := filepath.ToSlash(relOS)
				if validateRelPath(rel) == nil {
					if info, iErr := d.Info(); iErr == nil {
						result[rel] = info
					}
				}
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		relOS, err := filepath.Rel(ts.workspaceDir, path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relOS)
		if err := validateRelPath(rel); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		result[rel] = info
		return nil
	})
	return result, err
}

// safeTargetPathLocked validates rel, inspects every ancestor directory component with Lstat
// to reject symlink traversal escapes, and optionally creates missing parent directories.
func (ts *TreeStore) safeTargetPathLocked(rel string, createParents bool) (string, error) {
	if err := validateRelPath(rel); err != nil {
		return "", err
	}

	parts := strings.Split(rel, "/")
	cur := ts.workspaceDir

	for i := 0; i < len(parts)-1; i++ {
		cur = filepath.Join(cur, parts[i])
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				if createParents {
					if mkErr := os.Mkdir(cur, 0755); mkErr != nil {
						return "", fmt.Errorf("mkdir %s: %w", cur, mkErr)
					}
				}
				continue
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("security violation: refusing to traverse symlink directory %q", cur)
		}
		if !info.IsDir() {
			if createParents {
				if rmErr := os.Remove(cur); rmErr != nil {
					return "", rmErr
				}
				if mkErr := os.Mkdir(cur, 0755); mkErr != nil {
					return "", mkErr
				}
			}
		}
	}

	target := filepath.Join(cur, parts[len(parts)-1])
	if !isSubpath(ts.workspaceDir, target) || sameFilePath(ts.workspaceDir, target) {
		return "", fmt.Errorf("security violation: path %q escapes workspace", rel)
	}
	return target, nil
}

func (ts *TreeStore) pruneEmptyParentsLocked(dir string) {
	for !sameFilePath(dir, ts.workspaceDir) && isSubpath(ts.workspaceDir, dir) {
		base := filepath.Base(dir)
		if base == ".git" || base == ".traceport" {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func normalizeFileMode(mode fs.FileMode) uint32 {
	if runtime.GOOS == "windows" {
		return 0644
	}
	if mode&0111 != 0 {
		return 0755
	}
	return 0644
}

func validateRelPath(rel string) error {
	if rel == "" {
		return fmt.Errorf("empty relative path")
	}
	if strings.IndexByte(rel, 0) >= 0 {
		return fmt.Errorf("NUL byte in path")
	}
	if strings.Contains(rel, "\\") {
		return fmt.Errorf("backslash not allowed in canonical manifest path %q", rel)
	}
	if strings.HasPrefix(rel, "/") || filepath.VolumeName(rel) != "" {
		return fmt.Errorf("absolute path %q not allowed", rel)
	}

	parts := strings.Split(rel, "/")
	for i, p := range parts {
		if p == "" || p == "." || p == ".." {
			return fmt.Errorf("invalid path segment %q in %q", p, rel)
		}
		if i == 0 && (p == ".git" || p == ".traceport") {
			return fmt.Errorf("reserved directory %q in manifest path", p)
		}
	}
	return nil
}

func isValidSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func sameFilePath(a, b string) bool {
	ca := filepath.Clean(a)
	cb := filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

func isInternalStoreArtifact(name string) bool {
	if name == "stat_cache.json" {
		return true
	}
	if strings.HasSuffix(name, ".meta.json") ||
		strings.HasSuffix(name, ".frames.jsonl") ||
		strings.HasSuffix(name, ".manifest.zst") ||
		strings.HasSuffix(name, ".zdict") ||
		strings.HasSuffix(name, ".tmp") {
		return true
	}
	if strings.HasPrefix(name, "trace_") && strings.HasSuffix(name, ".json") {
		return true
	}
	return false
}

func writeAtomicFile(dest string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".tp-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, dest); err != nil {
		// Handle read-only target attribute on Windows.
		_ = os.Chmod(dest, 0600)
		if retryErr := os.Rename(tmpName, dest); retryErr != nil {
			return retryErr
		}
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dest, perm)
	}
	cleanup = false
	return nil
}
