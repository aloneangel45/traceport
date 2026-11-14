package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aloneangel45/traceport/pkg/checkpoint"
	"github.com/aloneangel45/traceport/pkg/model"
)

// FileDiffEntry summarizes a single file mutation between two snapshots or working tree.
type FileDiffEntry struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // added, modified, deleted
	Unified string `json:"unified,omitempty"`
}

// DiffReport contains structured and unified diff output for traceport diff.
type DiffReport struct {
	SessionID   string          `json:"session_id"`
	FromStep    int             `json:"from_step"`
	ToStep      int             `json:"to_step"` // 0 indicates live working tree
	FromTree    string          `json:"from_tree"`
	ToTree      string          `json:"to_tree"`
	FilesAdded  int             `json:"files_added"`
	FilesMod    int             `json:"files_modified"`
	FilesDel    int             `json:"files_deleted"`
	Changes     []FileDiffEntry `json:"changes"`
	UnifiedText string          `json:"unified_text"`
}

type diffBlobRef struct {
	sha256  string
	content []byte
}

// DiffSession computes a pure-Go filesystem and unified line diff between two steps of sessionID,
// or between fromStep and the current live workspaceDir when toStep <= 0.
func (e *Engine) DiffSession(sessionID string, fromStep, toStep int, workspaceDir string) (*DiffReport, error) {
	session, err := e.store.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if len(session.Frames) == 0 {
		return nil, fmt.Errorf("session %s contains no frames", sessionID)
	}

	if fromStep < 0 {
		return nil, fmt.Errorf("from step %d out of bounds (1..%d)", fromStep, len(session.Frames))
	}
	if toStep < 0 {
		return nil, fmt.Errorf("to step %d out of bounds (0..%d)", toStep, len(session.Frames))
	}
	if fromStep == 0 {
		fromStep = 1
	}
	if fromStep > len(session.Frames) {
		return nil, fmt.Errorf("from step %d out of bounds (1..%d)", fromStep, len(session.Frames))
	}
	if toStep > len(session.Frames) {
		return nil, fmt.Errorf("to step %d out of bounds (0..%d)", toStep, len(session.Frames))
	}

	workDir := workspaceDir
	if workDir == "" {
		workDir = session.WorkspaceDir
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	treeStore, err := checkpoint.NewTreeStore(workDir, e.store.RootDir())
	if err != nil {
		return nil, err
	}
	defer treeStore.Close()

	fromHash := resolveSnapshotAtOrBefore(session.Frames, fromStep)
	if fromHash == "" {
		return nil, fmt.Errorf("no workspace checkpoint found at or before step %d", fromStep)
	}
	fromManifest, err := treeStore.LoadManifest(fromHash)
	if err != nil {
		return nil, fmt.Errorf("load snapshot for step %d: %w", fromStep, err)
	}

	fromMap := make(map[string]diffBlobRef, len(fromManifest.Files))
	for _, f := range fromManifest.Files {
		fromMap[f.Path] = diffBlobRef{sha256: f.SHA256}
	}

	var toHash string
	toMap := make(map[string]diffBlobRef)

	if toStep > 0 {
		toHash = resolveSnapshotAtOrBefore(session.Frames, toStep)
		if toHash == "" {
			return nil, fmt.Errorf("no workspace checkpoint found at or before step %d", toStep)
		}
		toManifest, err := treeStore.LoadManifest(toHash)
		if err != nil {
			return nil, fmt.Errorf("load snapshot for step %d: %w", toStep, err)
		}
		for _, f := range toManifest.Files {
			toMap[f.Path] = diffBlobRef{sha256: f.SHA256}
		}
	} else {
		toHash = "working-tree"
		liveFiles, err := readLiveWorkspaceFiles(workDir, e.store.RootDir())
		if err != nil {
			return nil, err
		}
		toMap = liveFiles
	}

	allPathsMap := make(map[string]struct{}, len(fromMap)+len(toMap))
	for p := range fromMap {
		allPathsMap[p] = struct{}{}
	}
	for p := range toMap {
		allPathsMap[p] = struct{}{}
	}

	allPaths := make([]string, 0, len(allPathsMap))
	for p := range allPathsMap {
		allPaths = append(allPaths, p)
	}
	sort.Strings(allPaths)

	readRef := func(ref diffBlobRef) ([]byte, error) {
		if ref.content != nil {
			return ref.content, nil
		}
		return treeStore.ReadBlob(ref.sha256)
	}

	report := &DiffReport{
		SessionID: sessionID,
		FromStep:  fromStep,
		ToStep:    toStep,
		FromTree:  fromHash,
		ToTree:    toHash,
		Changes:   []FileDiffEntry{},
	}

	var unifiedBuilder strings.Builder

	for _, p := range allPaths {
		aRef, inA := fromMap[p]
		bRef, inB := toMap[p]

		if inA && inB && aRef.sha256 == bRef.sha256 {
			continue
		}

		switch {
		case !inA && inB:
			bBytes, err := readRef(bRef)
			if err != nil {
				return nil, err
			}
			u := formatFileUnifiedDiff("/dev/null", "b/"+p, nil, bBytes)
			report.FilesAdded++
			report.Changes = append(report.Changes, FileDiffEntry{Path: p, Status: "added", Unified: u})
			unifiedBuilder.WriteString(u)

		case inA && !inB:
			aBytes, err := readRef(aRef)
			if err != nil {
				return nil, err
			}
			u := formatFileUnifiedDiff("a/"+p, "/dev/null", aBytes, nil)
			report.FilesDel++
			report.Changes = append(report.Changes, FileDiffEntry{Path: p, Status: "deleted", Unified: u})
			unifiedBuilder.WriteString(u)

		default:
			aBytes, err := readRef(aRef)
			if err != nil {
				return nil, err
			}
			bBytes, err := readRef(bRef)
			if err != nil {
				return nil, err
			}
			u := formatFileUnifiedDiff("a/"+p, "b/"+p, aBytes, bBytes)
			report.FilesMod++
			report.Changes = append(report.Changes, FileDiffEntry{Path: p, Status: "modified", Unified: u})
			unifiedBuilder.WriteString(u)
		}
	}

	report.UnifiedText = unifiedBuilder.String()
	return report, nil
}

func resolveSnapshotAtOrBefore(frames []model.Frame, step int) string {
	if step > len(frames) {
		step = len(frames)
	}
	for i := step - 1; i >= 0; i-- {
		if frames[i].GitSnapshot != "" {
			return frames[i].GitSnapshot
		}
	}
	return ""
}

func readLiveWorkspaceFiles(workDir, storeRoot string) (map[string]diffBlobRef, error) {
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	absStore, _ := filepath.Abs(storeRoot)
	sameWorkStore := sameWorkspacePath(absWork, absStore)

	out := make(map[string]diffBlobRef)
	err = filepath.WalkDir(absWork, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == absWork {
			return nil
		}
		name := d.Name()
		if name == ".git" || name == ".traceport" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && (strings.HasPrefix(name, ".tp-tmp-") || strings.HasPrefix(name, ".tp-meta-") || strings.HasPrefix(name, ".tp-export-")) {
			return nil
		}
		if absStore != "" && !sameWorkStore && isPathWithinDir(absStore, path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if sameWorkStore {
			if d.IsDir() && (name == "blobs" || name == "trees" || name == "dicts" || name == "sessions") {
				return filepath.SkipDir
			}
			if !d.IsDir() && (name == "stat_cache.json" ||
				strings.HasSuffix(name, ".meta.json") ||
				strings.HasSuffix(name, ".frames.jsonl") ||
				strings.HasSuffix(name, ".manifest.zst") ||
				strings.HasSuffix(name, ".zdict") ||
				(strings.HasPrefix(name, "trace_") && strings.HasSuffix(name, ".json"))) {
				return nil
			}
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(absWork, path)
		if err != nil {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if data == nil {
			data = []byte{}
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = diffBlobRef{
			sha256:  hex.EncodeToString(sum[:]),
			content: data,
		}
		return nil
	})
	return out, err
}

func isPathWithinDir(parentDir, candidate string) bool {
	if sameWorkspacePath(parentDir, candidate) {
		return true
	}
	rel, err := filepath.Rel(parentDir, candidate)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func isBinaryPayload(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	sample := b
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	return bytes.IndexByte(sample, 0) >= 0 || !utf8.Valid(sample)
}

func splitTextLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" && len(b) == 1 && b[0] == '\n' {
		return []string{""}
	}
	return strings.Split(s, "\n")
}

type diffOpKind byte

const (
	opEqual diffOpKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind  diffOpKind
	text  string
	lineA int // 1-based line number in A
	lineB int // 1-based line number in B
}

func formatFileUnifiedDiff(labelA, labelB string, aBytes, bBytes []byte) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- %s\n+++ %s\n", labelA, labelB))

	if isBinaryPayload(aBytes) || isBinaryPayload(bBytes) {
		sb.WriteString(fmt.Sprintf("Binary files %s and %s differ\n", labelA, labelB))
		return sb.String()
	}

	aLines := splitTextLines(aBytes)
	bLines := splitTextLines(bBytes)
	ops := computeLineDiffOps(aLines, bLines)

	const contextLines = 3
	type hunkRange struct{ start, end int }
	var hunks []hunkRange

	for i, op := range ops {
		if op.kind == opEqual {
			continue
		}
		s := i - contextLines
		if s < 0 {
			s = 0
		}
		e := i + contextLines + 1
		if e > len(ops) {
			e = len(ops)
		}
		if len(hunks) > 0 && s <= hunks[len(hunks)-1].end {
			hunks[len(hunks)-1].end = e
		} else {
			hunks = append(hunks, hunkRange{start: s, end: e})
		}
	}

	for _, hr := range hunks {
		slice := ops[hr.start:hr.end]
		startA, countA := 0, 0
		startB, countB := 0, 0

		for _, op := range slice {
			switch op.kind {
			case opEqual:
				if startA == 0 {
					startA = op.lineA
				}
				if startB == 0 {
					startB = op.lineB
				}
				countA++
				countB++
			case opDelete:
				if startA == 0 {
					startA = op.lineA
				}
				countA++
			case opInsert:
				if startB == 0 {
					startB = op.lineB
				}
				countB++
			}
		}
		if countA == 0 {
			startA = 0
		} else if startA == 0 {
			startA = 1
		}
		if countB == 0 {
			startB = 0
		} else if startB == 0 {
			startB = 1
		}

		sb.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", startA, countA, startB, countB))
		for _, op := range slice {
			switch op.kind {
			case opEqual:
				sb.WriteString(" " + op.text + "\n")
			case opDelete:
				sb.WriteString("-" + op.text + "\n")
			case opInsert:
				sb.WriteString("+" + op.text + "\n")
			}
		}
	}

	return sb.String()
}

func computeLineDiffOps(a, b []string) []diffOp {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < (len(a)-prefix) && suffix < (len(b)-prefix) &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}

	ops := make([]diffOp, 0, len(a)+len(b))
	for i := 0; i < prefix; i++ {
		ops = append(ops, diffOp{kind: opEqual, text: a[i], lineA: i + 1, lineB: i + 1})
	}

	midA := a[prefix : len(a)-suffix]
	midB := b[prefix : len(b)-suffix]

	if len(midA)*len(midB) <= 4_000_000 {
		// Exact LCS dynamic programming on trimmed middle region.
		n, m := len(midA), len(midB)
		dp := make([][]int, n+1)
		for i := range dp {
			dp[i] = make([]int, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if midA[i] == midB[j] {
					dp[i][j] = dp[i+1][j+1] + 1
				} else if dp[i+1][j] >= dp[i][j+1] {
					dp[i][j] = dp[i+1][j]
				} else {
					dp[i][j] = dp[i][j+1]
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			if i < n && j < m && midA[i] == midB[j] {
				ops = append(ops, diffOp{kind: opEqual, text: midA[i], lineA: prefix + i + 1, lineB: prefix + j + 1})
				i++
				j++
			} else if j == m || (i < n && dp[i+1][j] >= dp[i][j+1]) {
				ops = append(ops, diffOp{kind: opDelete, text: midA[i], lineA: prefix + i + 1})
				i++
			} else {
				ops = append(ops, diffOp{kind: opInsert, text: midB[j], lineB: prefix + j + 1})
				j++
			}
		}
	} else {
		// Linear fallback for very large (>2000x2000 changed lines) diffs.
		for i, line := range midA {
			ops = append(ops, diffOp{kind: opDelete, text: line, lineA: prefix + i + 1})
		}
		for j, line := range midB {
			ops = append(ops, diffOp{kind: opInsert, text: line, lineB: prefix + j + 1})
		}
	}

	startA := len(a) - suffix
	startB := len(b) - suffix
	for k := 0; k < suffix; k++ {
		ops = append(ops, diffOp{
			kind:  opEqual,
			text:  a[startA+k],
			lineA: startA + k + 1,
			lineB: startB + k + 1,
		})
	}
	return ops
}
