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
	"time"

	"github.com/klauspost/compress/zstd"
)

// racyCleanWindow is the Git-style racy-clean grace period during which statCache hits
// are bypassed so rapid sub-tick / same-size edits or immediate post-restore edits
// never record stale SHA-256 digests.
const racyCleanWindow = 2 * time.Second

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

type persistedStatCache struct {
	Version    int                                  `json:"version"`
	Workspaces map[string]map[string]statCacheEntry `json:"workspaces"`
}

var statCacheFileMu sync.Mutex

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
	realStore     string
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
	realWork := resolveRealPath(absWork)

	absStore, err := filepath.Abs(storeDir)
	if err != nil {
		return nil, fmt.Errorf("resolve store path: %w", err)
	}

	for _, sub := range []string{"blobs", "trees", "dicts"} {
		if err := os.MkdirAll(filepath.Join(absStore, sub), 0700); err != nil {
			return nil, fmt.Errorf("initialize store directory %s: %w", sub, err)
		}
	}
	realStore := resolveRealPath(absStore)

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
		realStore:     realStore,
		mode:          cfg.mode,
		compressor:    comp,
		statCache:     make(map[string]statCacheEntry),
		knownBlobs:    make(map[string]bool),
		manifestCache: make(map[string]*TreeManifest),
	}
	ts.loadStatCacheLocked()
	return ts, nil
}

// ResolveRealPath resolves symlinks and Windows NTFS junctions to their canonical filesystem target.
func ResolveRealPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if eval, err := evalPlatformRealPath(abs); err == nil && eval != "" {
		return filepath.Clean(eval)
	}
	parent := filepath.Dir(abs)
	if parent != abs {
		if evalParent, err := evalPlatformRealPath(parent); err == nil && evalParent != "" {
			return filepath.Join(evalParent, filepath.Base(abs))
		}
	}
	return filepath.Clean(abs)
}

// IsLinkMode reports whether mode represents a symbolic link or Windows reparse/junction entry.
func IsLinkMode(mode fs.FileMode) bool {
	return isPlatformLinkMode(mode)
}

func resolveRealPath(p string) string {
	return ResolveRealPath(p)
}

func (ts *TreeStore) refreshRealPathsLocked() {
	ts.realWorkspace = resolveRealPath(ts.workspaceDir)
	ts.realStore = resolveRealPath(ts.storeDir)
}

func isRacyModTime(now, mtime time.Time) bool {
	diff := now.Sub(mtime)
	if diff < 0 {
		diff = -diff
	}
	return diff < racyCleanWindow
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

func (ts *TreeStore) workspaceCacheKey() string {
	clean := filepath.Clean(ts.realWorkspace)
	if runtime.GOOS == "windows" {
		return strings.ToLower(clean)
	}
	return clean
}

func (ts *TreeStore) loadStatCacheLocked() {
	statCacheFileMu.Lock()
	data, err := os.ReadFile(ts.statCachePath())
	statCacheFileMu.Unlock()
	if err != nil || len(data) == 0 {
		return
	}
	var persisted persistedStatCache
	if err := json.Unmarshal(data, &persisted); err != nil || persisted.Version < 2 || persisted.Workspaces == nil {
		return
	}
	if wsEntries, ok := persisted.Workspaces[ts.workspaceCacheKey()]; ok && wsEntries != nil {
		cp := make(map[string]statCacheEntry, len(wsEntries))
		for k, v := range wsEntries {
			cp[k] = v
		}
		ts.statCache = cp
	}
}

func (ts *TreeStore) saveStatCacheLocked() {
	statCacheFileMu.Lock()
	defer statCacheFileMu.Unlock()

	var persisted persistedStatCache
	if existing, err := os.ReadFile(ts.statCachePath()); err == nil && len(existing) > 0 {
		if uErr := json.Unmarshal(existing, &persisted); uErr != nil || persisted.Version < 2 {
			persisted = persistedStatCache{}
		}
	}
	if persisted.Workspaces == nil {
		persisted.Workspaces = make(map[string]map[string]statCacheEntry)
	}
	persisted.Version = 2

	cp := make(map[string]statCacheEntry, len(ts.statCache))
	for k, v := range ts.statCache {
		cp[k] = v
	}
	persisted.Workspaces[ts.workspaceCacheKey()] = cp

	data, err := json.Marshal(persisted)
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
		encoded, encErr := ts.encodeBytesLocked(canonicalBytes)
		if encErr != nil {
			return "", stats, fmt.Errorf("encode manifest %s: %w", treeHash, encErr)
		}
		if err := writeAtomicFile(manifestFile, encoded, 0600); err != nil {
			return "", stats, fmt.Errorf("write manifest %s: %w", treeHash, err)
		}
	} else if ts.mode != ModeUncompressed && ts.compressor != nil {
		// Ensure closed compressor fails closed even if an identical manifest already existed on disk.
		if _, encErr := ts.compressor.Compress(nil); encErr != nil {
			return "", stats, encErr
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

func (ts *TreeStore) encodeBytesLocked(raw []byte) ([]byte, error) {
	if ts.mode == ModeUncompressed {
		cp := make([]byte, len(raw))
		copy(cp, raw)
		return cp, nil
	}
	return ts.compressor.Compress(raw)
}

func readFileConsistent(path string, initialInfo fs.FileInfo) ([]byte, fs.FileInfo, bool, error) {
	preInfo := initialInfo
	const maxAttempts = 4
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 || preInfo == nil {
			st, err := os.Lstat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, nil, false, nil
				}
				return nil, nil, false, err
			}
			if !st.Mode().IsRegular() {
				return nil, nil, false, nil
			}
			preInfo = st
		}

		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil, false, nil
			}
			return nil, nil, false, err
		}
		data, readErr := io.ReadAll(f)
		postInfo, statErr := f.Stat()
		_ = f.Close()
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return nil, nil, false, nil
			}
			return nil, nil, false, readErr
		}
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return nil, nil, false, nil
			}
			return nil, nil, false, statErr
		}
		if !postInfo.Mode().IsRegular() {
			return nil, nil, false, nil
		}

		sameStat := preInfo.Size() == postInfo.Size() &&
			preInfo.ModTime().Equal(postInfo.ModTime()) &&
			normalizeFileMode(preInfo.Mode()) == normalizeFileMode(postInfo.Mode()) &&
			int64(len(data)) == postInfo.Size()
		if sameStat || attempt == maxAttempts-1 {
			return data, postInfo, true, nil
		}
	}
	return nil, nil, false, nil
}

func (ts *TreeStore) scanWorkspaceLocked(stats *SnapshotStats) ([]FileEntry, error) {
	ts.refreshRealPathsLocked()

	workRoot := ts.realWorkspace
	storeRoot := ts.realStore
	storeInsideWork := isSubpath(workRoot, storeRoot) && !sameFilePath(workRoot, storeRoot)
	sameWorkAndStore := sameFilePath(workRoot, storeRoot)
	now := time.Now()

	var entries []FileEntry

	err := filepath.WalkDir(workRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if path == workRoot {
			return nil
		}

		// Exclude store directory if located inside workspace.
		if storeInsideWork && (sameFilePath(path, storeRoot) || isSubpath(storeRoot, path)) {
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
		if !d.IsDir() && isTransientTempFile(name) {
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

		// Never follow symlinks inside workspace: prevents workspace escape exfiltration.
		if IsLinkMode(d.Type()) {
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

		relOS, err := filepath.Rel(workRoot, path)
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

		if !isRacyModTime(now, info.ModTime()) {
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
		}

		stats.CacheMisses++
		data, postInfo, exists, err := readFileConsistent(path, info)
		if err != nil {
			return fmt.Errorf("read workspace file %s: %w", rel, err)
		}
		if !exists {
			return nil
		}
		stats.BytesRead += int64(len(data))

		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])

		if !ts.hasBlobLocked(sha) {
			blobFile := ts.blobPath(sha)
			if err := os.MkdirAll(filepath.Dir(blobFile), 0700); err != nil {
				return fmt.Errorf("create blob fanout dir: %w", err)
			}
			compressed, encErr := ts.encodeBytesLocked(data)
			if encErr != nil {
				return fmt.Errorf("compress blob %s: %w", sha, encErr)
			}
			if err := writeAtomicFile(blobFile, compressed, 0600); err != nil {
				return fmt.Errorf("write blob %s: %w", sha, err)
			}
			ts.knownBlobs[sha] = true
			stats.BlobsWritten++
		}

		realSize := int64(len(data))
		postMtimeNS := postInfo.ModTime().UnixNano()
		postNormMode := normalizeFileMode(postInfo.Mode())
		ts.statCache[rel] = statCacheEntry{
			ModTimeUnixNano: postMtimeNS,
			Size:            realSize,
			Mode:            postNormMode,
			SHA256:          sha,
		}
		entries = append(entries, FileEntry{
			Path:   rel,
			SHA256: sha,
			Size:   realSize,
			Mode:   postNormMode,
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

	var decoded []byte
	rawSum := sha256.Sum256(raw)
	if hex.EncodeToString(rawSum[:]) == treeHash {
		decoded = raw
	} else {
		decoded, err = ts.compressor.DecompressAuto(raw)
		if err != nil {
			return nil, fmt.Errorf("decompress manifest %s: %w", treeHash, err)
		}
		sum := sha256.Sum256(decoded)
		actualHash := hex.EncodeToString(sum[:])
		if actualHash != treeHash {
			return nil, fmt.Errorf("manifest %s digest mismatch: got %s", treeHash, actualHash)
		}
	}

	var manifest TreeManifest
	if err := json.Unmarshal(decoded, &manifest); err != nil {
		return nil, fmt.Errorf("unmarshal manifest %s: %w", treeHash, err)
	}

	seenPaths := make(map[string]struct{}, len(manifest.Files))
	for _, f := range manifest.Files {
		if err := validateRelPath(f.Path); err != nil {
			return nil, fmt.Errorf("manifest %s contains unsafe path %q: %w", treeHash, f.Path, err)
		}
		if _, dup := seenPaths[f.Path]; dup {
			return nil, fmt.Errorf("manifest %s contains duplicate path %q", treeHash, f.Path)
		}
		seenPaths[f.Path] = struct{}{}
		if !isValidSHA256Hex(f.SHA256) {
			return nil, fmt.Errorf("manifest %s contains invalid blob hash %q", treeHash, f.SHA256)
		}
		if f.Size < 0 {
			return nil, fmt.Errorf("manifest %s contains negative file size %d for %q", treeHash, f.Size, f.Path)
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
	return ts.readBlobBySHA(sha)
}

func (ts *TreeStore) readBlobBySHA(sha string) ([]byte, error) {
	rawBlobBytes, err := os.ReadFile(ts.blobPath(sha))
	if err != nil {
		return nil, fmt.Errorf("blob %s missing: %w", sha, err)
	}

	// If sha256(rawBlobBytes) already matches sha (e.g. ModeUncompressed storing a user .zst file
	// whose payload begins with ZstdMagic), return rawBlobBytes directly without DecompressAuto.
	rawSum := sha256.Sum256(rawBlobBytes)
	if hex.EncodeToString(rawSum[:]) == sha {
		cp := make([]byte, len(rawBlobBytes))
		copy(cp, rawBlobBytes)
		return cp, nil
	}

	plain, err := ts.compressor.DecompressAuto(rawBlobBytes)
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

func buildTrackedDirSet(files []FileEntry) map[string]bool {
	dirs := map[string]bool{".": true}
	for _, f := range files {
		idx := strings.LastIndexByte(f.Path, '/')
		for idx > 0 {
			d := f.Path[:idx]
			if dirs[d] {
				break
			}
			dirs[d] = true
			idx = strings.LastIndexByte(d, '/')
		}
	}
	return dirs
}

func symlinkCollidesOrInNewDir(rel string, trackedFiles map[string]FileEntry, trackedDirs map[string]bool) bool {
	if _, ok := trackedFiles[rel]; ok {
		return true
	}
	if trackedDirs[rel] {
		return true
	}
	for cur := rel; ; {
		slash := strings.LastIndexByte(cur, '/')
		if slash <= 0 {
			break
		}
		cur = cur[:slash]
		if _, ok := trackedFiles[cur]; ok {
			return true
		}
	}
	parent := "."
	if slash := strings.LastIndexByte(rel, '/'); slash > 0 {
		parent = rel[:slash]
	}
	return !trackedDirs[parent]
}

// PlanRestore computes a dry-run preview of workspace changes required to reach treeHash.
func (ts *TreeStore) PlanRestore(treeHash string) (*RestorePlan, error) {
	manifest, err := ts.LoadManifest(treeHash)
	if err != nil {
		return nil, err
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.refreshRealPathsLocked()

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

	trackedDirs := buildTrackedDirSet(manifest.Files)
	for rel, info := range diskFiles {
		if _, keep := targetSet[rel]; !keep {
			if IsLinkMode(info.Mode()) && !symlinkCollidesOrInNewDir(rel, targetSet, trackedDirs) {
				continue
			}
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

	ts.refreshRealPathsLocked()
	workRoot := ts.realWorkspace

	stats := &RestoreStats{TreeHash: treeHash}

	targetMap := make(map[string]FileEntry, len(manifest.Files))
	for _, e := range manifest.Files {
		if err := validateRelPath(e.Path); err != nil {
			return nil, err
		}
		targetMap[e.Path] = e
	}
	trackedDirs := buildTrackedDirSet(manifest.Files)

	diskFiles, err := ts.listCurrentWorkspacePathsLocked()
	if err != nil {
		return nil, err
	}

	// 1. Delete untracked post-checkpoint regular files AND colliding / new-dir symlinks BEFORE
	// restoring files underneath them (so e.g. a symlink replacing 'src/' is unlinked safely
	// without ever traversing into its external target). Pre-existing non-colliding workspace
	// symlinks are preserved untouched.
	var toDelete []string
	for rel, info := range diskFiles {
		if _, ok := targetMap[rel]; !ok {
			if IsLinkMode(info.Mode()) && !symlinkCollidesOrInNewDir(rel, targetMap, trackedDirs) {
				continue
			}
			toDelete = append(toDelete, rel)
		}
	}
	sort.Strings(toDelete)

	for _, rel := range toDelete {
		targetPath, err := ts.safeTargetPathLocked(rel, false)
		if err != nil {
			return nil, err
		}
		parentDir := filepath.Dir(targetPath)
		if isSubpath(workRoot, parentDir) && !sameFilePath(parentDir, workRoot) {
			_ = os.Chmod(parentDir, 0755)
		}
		if linfo, lerr := os.Lstat(targetPath); lerr == nil && !IsLinkMode(linfo.Mode()) {
			_ = os.Chmod(targetPath, 0600)
		}
		if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove post-checkpoint file %s: %w", rel, err)
		}
		delete(ts.statCache, rel)
		stats.FilesDeleted++
		ts.pruneEmptyParentsLocked(parentDir)
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

		// If target is currently a directory or symlink, remove/unlink it before atomic replacement.
		if linfo, err := os.Lstat(targetPath); err == nil {
			if IsLinkMode(linfo.Mode()) {
				if err := os.Remove(targetPath); err != nil {
					return nil, fmt.Errorf("remove symlink %s: %w", entry.Path, err)
				}
			} else if linfo.IsDir() {
				makeTreeWritableNoSymlink(targetPath)
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

	ts.pruneUntrackedEmptyDirsLocked()
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

	var decodedManifest []byte
	rawSum := sha256.Sum256(rawManifest)
	if hex.EncodeToString(rawSum[:]) == treeHash {
		decodedManifest = rawManifest
	} else {
		decodedManifest, err = ts.compressor.DecompressAuto(rawManifest)
		if err != nil {
			return nil, fmt.Errorf("manifest %s corrupt zstd stream: %w", treeHash, err)
		}
		sum := sha256.Sum256(decodedManifest)
		actualTreeHash := hex.EncodeToString(sum[:])
		if actualTreeHash != treeHash {
			return nil, fmt.Errorf("manifest %s digest mismatch: got %s", treeHash, actualTreeHash)
		}
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
	return ts.readBlobBySHA(sha)
}

func (ts *TreeStore) fileMatchesEntryLocked(entry FileEntry, info fs.FileInfo) bool {
	if !info.Mode().IsRegular() || info.Size() != entry.Size {
		return false
	}
	mtimeNS := info.ModTime().UnixNano()
	normMode := normalizeFileMode(info.Mode())

	if !isRacyModTime(time.Now(), info.ModTime()) {
		if cached, ok := ts.statCache[entry.Path]; ok &&
			cached.ModTimeUnixNano == mtimeNS &&
			cached.Size == info.Size() &&
			cached.Mode == normMode &&
			cached.SHA256 == entry.SHA256 {
			return true
		}
	}

	fullPath := filepath.Join(ts.realWorkspace, filepath.FromSlash(entry.Path))
	f, err := os.Open(fullPath)
	if err != nil {
		return false
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	postInfo, statErr := f.Stat()
	_ = f.Close()
	if copyErr != nil || statErr != nil || !postInfo.Mode().IsRegular() || postInfo.Size() != entry.Size {
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
		ModTimeUnixNano: postInfo.ModTime().UnixNano(),
		Size:            postInfo.Size(),
		Mode:            normalizeFileMode(postInfo.Mode()),
		SHA256:          diskSHA,
	}
	return true
}

func (ts *TreeStore) listCurrentWorkspacePathsLocked() (map[string]fs.FileInfo, error) {
	workRoot := ts.realWorkspace
	storeRoot := ts.realStore
	storeInsideWork := isSubpath(workRoot, storeRoot) && !sameFilePath(workRoot, storeRoot)
	sameWorkAndStore := sameFilePath(workRoot, storeRoot)
	result := make(map[string]fs.FileInfo)

	err := filepath.WalkDir(workRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if path == workRoot {
			return nil
		}
		if storeInsideWork && (sameFilePath(path, storeRoot) || isSubpath(storeRoot, path)) {
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
		if !d.IsDir() && isTransientTempFile(name) {
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

		if IsLinkMode(d.Type()) {
			relOS, err := filepath.Rel(workRoot, path)
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

		relOS, err := filepath.Rel(workRoot, path)
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

	workRoot := ts.realWorkspace
	parts := strings.Split(rel, "/")
	cur := workRoot

	for i := 0; i < len(parts)-1; i++ {
		parent := cur
		cur = filepath.Join(cur, parts[i])
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				if createParents {
					if mkErr := os.Mkdir(cur, 0755); mkErr != nil {
						_ = os.Chmod(parent, 0755)
						if retryErr := os.Mkdir(cur, 0755); retryErr != nil {
							return "", fmt.Errorf("mkdir %s: %w", cur, retryErr)
						}
					}
				}
				continue
			}
			return "", err
		}
		if IsLinkMode(info.Mode()) {
			return "", fmt.Errorf("security violation: refusing to traverse symlink directory %q", cur)
		}
		if !info.IsDir() {
			if createParents {
				_ = os.Chmod(parent, 0755)
				_ = os.Chmod(cur, 0600)
				if rmErr := os.Remove(cur); rmErr != nil {
					return "", rmErr
				}
				if mkErr := os.Mkdir(cur, 0755); mkErr != nil {
					return "", mkErr
				}
			}
		} else if createParents && runtime.GOOS != "windows" && info.Mode().Perm()&0200 == 0 {
			_ = os.Chmod(cur, info.Mode().Perm()|0700)
		}
	}

	if createParents && !sameFilePath(cur, workRoot) {
		if dInfo, dErr := os.Lstat(cur); dErr == nil && dInfo.IsDir() && runtime.GOOS != "windows" && dInfo.Mode().Perm()&0200 == 0 {
			_ = os.Chmod(cur, dInfo.Mode().Perm()|0700)
		}
	}

	target := filepath.Join(cur, parts[len(parts)-1])
	if !isSubpath(workRoot, target) || sameFilePath(workRoot, target) {
		return "", fmt.Errorf("security violation: path %q escapes workspace", rel)
	}
	return target, nil
}

func (ts *TreeStore) pruneEmptyParentsLocked(dir string) {
	workRoot := ts.realWorkspace
	for !sameFilePath(dir, workRoot) && isSubpath(workRoot, dir) {
		base := filepath.Base(dir)
		if base == ".git" || base == ".traceport" {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		parent := filepath.Dir(dir)
		if isSubpath(workRoot, parent) && !sameFilePath(parent, workRoot) {
			_ = os.Chmod(parent, 0755)
		}
		_ = os.Chmod(dir, 0700)
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = parent
	}
}

func (ts *TreeStore) pruneUntrackedEmptyDirsLocked() {
	workRoot := ts.realWorkspace
	storeRoot := ts.realStore
	storeInsideWork := isSubpath(workRoot, storeRoot) && !sameFilePath(workRoot, storeRoot)
	sameWorkAndStore := sameFilePath(workRoot, storeRoot)

	var dirs []string
	_ = filepath.WalkDir(workRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == workRoot {
			return nil
		}
		if storeInsideWork && (sameFilePath(path, storeRoot) || isSubpath(storeRoot, path)) {
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
		if !d.IsDir() && isTransientTempFile(name) {
			return nil
		}
		if sameWorkAndStore && d.IsDir() && (name == "blobs" || name == "trees" || name == "dicts" || name == "sessions") {
			return filepath.SkipDir
		}
		if IsLinkMode(d.Type()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})

	// Remove empty directories deepest-first.
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			continue
		}
		parent := filepath.Dir(dir)
		if isSubpath(workRoot, parent) && !sameFilePath(parent, workRoot) {
			_ = os.Chmod(parent, 0755)
		}
		_ = os.Chmod(dir, 0700)
		_ = os.Remove(dir)
	}
}

func makeTreeWritableNoSymlink(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if IsLinkMode(d.Type()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(path, 0700)
		} else {
			_ = os.Chmod(path, 0600)
		}
		return nil
	})
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
	if len(rel) >= 2 && ((rel[0] >= 'a' && rel[0] <= 'z') || (rel[0] >= 'A' && rel[0] <= 'Z')) && rel[1] == ':' {
		return fmt.Errorf("windows drive path %q not allowed", rel)
	}
	if strings.Contains(rel, ":") {
		return fmt.Errorf("colon not allowed in canonical manifest path %q", rel)
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

func isTransientTempFile(name string) bool {
	return strings.HasPrefix(name, ".tp-tmp-") ||
		strings.HasPrefix(name, ".tp-meta-") ||
		strings.HasPrefix(name, ".tp-export-")
}

func isInternalStoreArtifact(name string) bool {
	if name == "stat_cache.json" || isTransientTempFile(name) {
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
			_ = os.Chmod(dest, 0600)
			time.Sleep(25 * time.Millisecond)
		}
		if renameErr = os.Rename(tmpName, dest); renameErr == nil {
			break
		}
	}
	if renameErr != nil {
		return renameErr
	}
	if runtime.GOOS != "windows" || perm&0200 == 0 {
		_ = os.Chmod(dest, perm)
	}
	cleanup = false
	return nil
}
