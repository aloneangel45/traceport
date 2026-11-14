package replay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
)

// GCOptions configures session pruning and unreferenced zstd blob sweeping.
type GCOptions struct {
	Keep   int  // -1 retains all sessions; >= 0 retains newest Keep sessions
	DryRun bool // preview reclaimed bytes without deleting files
}

// GCReport summarizes pruned sessions, swept zstd blobs, and retained compression metrics.
type GCReport struct {
	DryRun                  bool    `json:"dry_run"`
	SessionsRetained        int     `json:"sessions_retained"`
	SessionsPruned          int     `json:"sessions_pruned"`
	TreesRetained           int     `json:"trees_retained"`
	TreesRemoved            int     `json:"trees_removed"`
	BlobsRetained           int     `json:"blobs_retained"`
	BlobsRemoved            int     `json:"blobs_removed"`
	BytesReclaimed          int64   `json:"bytes_reclaimed"`
	RetainedCompressedBytes int64   `json:"retained_compressed_bytes"`
	RetainedLogicalBytes    int64   `json:"retained_logical_bytes"`
	CompressionRatioPct     float64 `json:"compression_ratio_pct"`
}

// GC prunes old sessions beyond opts.Keep (when Keep >= 0) and sweeps unreferenced
// Zstandard manifests and content-addressed blobs from the TracePort store.
func (e *Engine) GC(opts GCOptions) (*GCReport, error) {
	allIDs, err := e.store.ListSessionIDs()
	if err != nil {
		return nil, err
	}

	sessions, err := e.store.ListSessions()
	if err != nil {
		return nil, err
	}

	loadedIDs := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		loadedIDs[s.ID] = true
	}
	// Never sweep manifests or blobs if any session on disk failed to decode; otherwise GC would
	// treat that session's referenced trees and blobs as unreachable and permanently delete them.
	for _, id := range allIDs {
		if !loadedIDs[id] {
			_, loadErr := e.store.GetSession(id)
			return nil, fmt.Errorf("refusing garbage collection: session %s is unreadable (%v); run 'traceport verify' before gc", id, loadErr)
		}
	}

	report := &GCReport{
		DryRun: opts.DryRun,
	}

	retained := sessions
	var prunedIDs []string
	if opts.Keep >= 0 && len(sessions) > opts.Keep {
		retained = sessions[:opts.Keep]
		for _, s := range sessions[opts.Keep:] {
			prunedIDs = append(prunedIDs, s.ID)
		}
	}
	report.SessionsRetained = len(retained)
	report.SessionsPruned = len(prunedIDs)

	// Tally bytes of pruned session files and remove if !DryRun.
	baseDir := e.store.BaseDir()
	for _, id := range prunedIDs {
		for _, p := range []string{
			filepath.Join(baseDir, id+".meta.json"),
			filepath.Join(baseDir, id+".frames.jsonl"),
			filepath.Join(baseDir, id+".json"),
		} {
			if info, sErr := os.Stat(p); sErr == nil && !info.IsDir() {
				report.BytesReclaimed += info.Size()
			}
		}
		if !opts.DryRun {
			_ = e.store.DeleteSession(id)
		}
	}

	// Collect reachable tree hashes across retained sessions.
	reachableTrees := make(map[string]bool)
	for _, s := range retained {
		for _, fr := range s.Frames {
			if fr.GitSnapshot != "" {
				reachableTrees[fr.GitSnapshot] = true
			}
		}
		if s.SafetySnapshot != "" {
			reachableTrees[s.SafetySnapshot] = true
		}
	}

	rootDir := e.store.RootDir()
	treeStore, err := checkpoint.NewTreeStore(rootDir, rootDir)
	if err != nil {
		return nil, err
	}
	defer treeStore.Close()

	// Collect reachable blob SHA-256 digests and logical uncompressed bytes.
	reachableBlobs := make(map[string]int64)
	for treeHash := range reachableTrees {
		if !treeStore.HasTree(treeHash) {
			continue
		}
		manifest, mErr := treeStore.LoadManifest(treeHash)
		if mErr != nil {
			return nil, fmt.Errorf("refusing garbage collection: retained manifest %s failed to load: %w", treeHash, mErr)
		}
		for _, f := range manifest.Files {
			reachableBlobs[f.SHA256] = f.Size
		}
	}

	// Sweep unreferenced manifests in <rootDir>/trees/.
	treesDir := filepath.Join(rootDir, "trees")
	if tEntries, tErr := os.ReadDir(treesDir); tErr == nil {
		for _, te := range tEntries {
			if te.IsDir() || !strings.HasSuffix(te.Name(), ".manifest.zst") {
				continue
			}
			hash := strings.TrimSuffix(te.Name(), ".manifest.zst")
			fullPath := filepath.Join(treesDir, te.Name())
			var fSize int64
			if info, iErr := te.Info(); iErr == nil {
				fSize = info.Size()
			}
			if reachableTrees[hash] {
				report.TreesRetained++
				report.RetainedCompressedBytes += fSize
			} else {
				report.TreesRemoved++
				report.BytesReclaimed += fSize
				if !opts.DryRun {
					removeWritableFile(fullPath)
				}
			}
		}
	}

	// Sweep unreferenced zstd blobs in <rootDir>/blobs/<fanout>/*.zst.
	blobsDir := filepath.Join(rootDir, "blobs")
	if fanouts, bErr := os.ReadDir(blobsDir); bErr == nil {
		for _, fan := range fanouts {
			if !fan.IsDir() || len(fan.Name()) != 2 {
				continue
			}
			fanDir := filepath.Join(blobsDir, fan.Name())
			bEntries, rErr := os.ReadDir(fanDir)
			if rErr != nil {
				continue
			}
			removedInFan := 0
			for _, be := range bEntries {
				if be.IsDir() || !strings.HasSuffix(be.Name(), ".zst") {
					continue
				}
				sha := fan.Name() + strings.TrimSuffix(be.Name(), ".zst")
				fullPath := filepath.Join(fanDir, be.Name())
				var fSize int64
				if info, iErr := be.Info(); iErr == nil {
					fSize = info.Size()
				}
				if logicalSize, keep := reachableBlobs[sha]; keep {
					report.BlobsRetained++
					report.RetainedCompressedBytes += fSize
					report.RetainedLogicalBytes += logicalSize
				} else {
					report.BlobsRemoved++
					report.BytesReclaimed += fSize
					removedInFan++
					if !opts.DryRun {
						removeWritableFile(fullPath)
					}
				}
			}
			if !opts.DryRun && removedInFan == len(bEntries) {
				_ = os.Chmod(fanDir, 0700)
				_ = os.Remove(fanDir)
			}
		}
	}

	if !opts.DryRun && report.BlobsRemoved > 0 {
		// Remove stat_cache.json so subsequent snapshots rebuild clean blob references.
		removeWritableFile(filepath.Join(treesDir, "stat_cache.json"))
	}

	if report.RetainedLogicalBytes > 0 {
		report.CompressionRatioPct = float64(report.RetainedCompressedBytes) * 100.0 / float64(report.RetainedLogicalBytes)
	}

	return report, nil
}

func removeWritableFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_ = os.Chmod(path, 0600)
		_ = os.Remove(path)
	}
}
