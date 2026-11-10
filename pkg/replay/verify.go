package replay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/storage"
)

// VerifyReport summarizes cryptographic integrity verification of sessions, manifests, and blobs.
type VerifyReport struct {
	Valid           bool     `json:"valid"`
	SessionsChecked int      `json:"sessions_checked"`
	FramesChecked   int      `json:"frames_checked"`
	TreesVerified   int      `json:"trees_verified"`
	FilesVerified   int      `json:"files_verified"`
	BytesVerified   int64    `json:"bytes_verified"`
	Errors          []string `json:"errors,omitempty"`
}

// Verify checks cryptographic SHA-256 integrity of sessionID (or the entire store if sessionID == "").
func (e *Engine) Verify(sessionID string) (*VerifyReport, error) {
	report := &VerifyReport{Valid: true}

	treeStore, err := checkpoint.NewTreeStore(e.store.RootDir(), e.store.RootDir())
	if err != nil {
		return nil, err
	}
	defer treeStore.Close()

	checkedTrees := make(map[string]bool)
	verifyTreeOnce := func(hash, contextLabel string) {
		if hash == "" || checkedTrees[hash] {
			return
		}
		checkedTrees[hash] = true

		// Legacy 40-char git hashes are not stored in trees/; only verify 64-char zstd manifests
		// or report missing if 64-char hash is absent from trees/.
		if len(hash) == 40 && checkpoint.IsValidGitHash(hash) && !treeStore.HasTree(hash) {
			return
		}

		res, vErr := treeStore.VerifyTree(hash)
		if vErr != nil {
			report.Valid = false
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", contextLabel, vErr))
			return
		}
		report.TreesVerified++
		report.FilesVerified += res.FilesVerified
		report.BytesVerified += res.BytesVerified
	}

	checkSessionByID := func(id string) error {
		sess, sErr := e.store.GetSession(id)
		if sErr != nil {
			report.Valid = false
			report.Errors = append(report.Errors, fmt.Sprintf("session %s: %v", id, sErr))
			return sErr
		}
		report.SessionsChecked++
		report.FramesChecked += len(sess.Frames)

		for _, fr := range sess.Frames {
			if fr.GitSnapshot != "" {
				verifyTreeOnce(fr.GitSnapshot, fmt.Sprintf("session %s step %d", sess.ID, fr.Index))
			}
		}
		if sess.SafetySnapshot != "" {
			verifyTreeOnce(sess.SafetySnapshot, fmt.Sprintf("session %s safety_snapshot", sess.ID))
		}
		return nil
	}

	if sessionID != "" {
		if err := storage.ValidateSessionID(sessionID); err != nil {
			return nil, err
		}
		if _, err := e.store.GetSession(sessionID); err != nil {
			return nil, err
		}
		_ = checkSessionByID(sessionID)
		return report, nil
	}

	// Full store verification: inspect all session files on disk so corrupt sessions are detected too.
	entries, err := os.ReadDir(e.store.BaseDir())
	if err != nil {
		return nil, err
	}
	seenIDs := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		var id string
		switch {
		case strings.HasSuffix(name, ".meta.json"):
			id = strings.TrimSuffix(name, ".meta.json")
		case strings.HasSuffix(name, ".json") && name != "stat_cache.json":
			id = strings.TrimSuffix(name, ".json")
		default:
			continue
		}
		if storage.ValidateSessionID(id) == nil && !seenIDs[id] {
			seenIDs[id] = true
			_ = checkSessionByID(id)
		}
	}

	// Also verify any standalone manifests in <rootDir>/trees/.
	treesDir := filepath.Join(e.store.RootDir(), "trees")
	if tEntries, tErr := os.ReadDir(treesDir); tErr == nil {
		for _, te := range tEntries {
			if te.IsDir() || !strings.HasSuffix(te.Name(), ".manifest.zst") {
				continue
			}
			hash := strings.TrimSuffix(te.Name(), ".manifest.zst")
			verifyTreeOnce(hash, "standalone manifest "+hash)
		}
	}

	return report, nil
}
