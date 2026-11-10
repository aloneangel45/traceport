package checkpoint

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var validGitTreeHashRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// IsValidGitHash validates legacy 40-char SHA-1 or 64-char SHA-256 Git object hashes,
// rejecting option injection (leading '-') or non-hex refs.
func IsValidGitHash(hash string) bool {
	return validGitTreeHashRe.MatchString(hash)
}

// WorkspaceManager provides backward-compatible Git write-tree snapshot support
// using an isolated temporary GIT_INDEX_FILE so .git/index is never mutated.
type WorkspaceManager struct {
	workspaceDir string
}

// NewWorkspaceManager creates a Git-backed legacy WorkspaceManager.
func NewWorkspaceManager(dir string) *WorkspaceManager {
	return &WorkspaceManager{workspaceDir: dir}
}

func (w *WorkspaceManager) gitEnvWithTempIndex(seedFromRepoIndex bool) (env []string, cleanup func(), err error) {
	tmpDir, err := os.MkdirTemp("", "traceport-git-idx-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp git index dir: %w", err)
	}
	cleanupFn := func() {
		_ = os.RemoveAll(tmpDir)
	}

	tmpIdx := filepath.Join(tmpDir, "index")
	if seedFromRepoIndex {
		gitDirCmd := exec.Command("git", "rev-parse", "--git-path", "index")
		gitDirCmd.Dir = w.workspaceDir
		if out, gErr := gitDirCmd.Output(); gErr == nil {
			repoIdx := strings.TrimSpace(string(out))
			if !filepath.IsAbs(repoIdx) {
				repoIdx = filepath.Join(w.workspaceDir, repoIdx)
			}
			if data, rErr := os.ReadFile(repoIdx); rErr == nil && len(data) > 0 {
				_ = os.WriteFile(tmpIdx, data, 0600)
			}
		}
	}

	baseEnv := os.Environ()
	filtered := make([]string, 0, len(baseEnv)+1)
	for _, kv := range baseEnv {
		if !strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			filtered = append(filtered, kv)
		}
	}
	filtered = append(filtered, "GIT_INDEX_FILE="+tmpIdx)
	return filtered, cleanupFn, nil
}

// CreateSnapshot creates an instantaneous git write-tree snapshot using an isolated
// temporary GIT_INDEX_FILE without creating a commit or mutating .git/index.
func (w *WorkspaceManager) CreateSnapshot() (string, error) {
	env, cleanup, err := w.gitEnvWithTempIndex(true)
	if err != nil {
		return "", err
	}
	defer cleanup()

	addCmd := exec.Command("git", "-c", "core.autocrlf=false", "add", "-A")
	addCmd.Dir = w.workspaceDir
	addCmd.Env = env
	if err := addCmd.Run(); err != nil {
		return "", fmt.Errorf("git add failed: %w", err)
	}

	treeCmd := exec.Command("git", "write-tree")
	treeCmd.Dir = w.workspaceDir
	treeCmd.Env = env
	var out bytes.Buffer
	treeCmd.Stdout = &out

	if err := treeCmd.Run(); err != nil {
		return "", fmt.Errorf("git write-tree failed: %w", err)
	}

	treeHash := strings.TrimSpace(out.String())
	if !IsValidGitHash(treeHash) {
		return "", fmt.Errorf("invalid git tree hash returned: %q", treeHash)
	}
	return treeHash, nil
}

// RestoreSnapshot checks out the workspace files to the exact state saved in treeHash
// using an isolated temporary GIT_INDEX_FILE without mutating .git/index.
func (w *WorkspaceManager) RestoreSnapshot(treeHash string) error {
	if !IsValidGitHash(treeHash) {
		return fmt.Errorf("invalid git tree hash %q", treeHash)
	}

	env, cleanup, err := w.gitEnvWithTempIndex(false)
	if err != nil {
		return err
	}
	defer cleanup()

	readCmd := exec.Command("git", "read-tree", "--", treeHash)
	readCmd.Dir = w.workspaceDir
	readCmd.Env = env
	if err := readCmd.Run(); err != nil {
		return fmt.Errorf("git read-tree failed: %w", err)
	}

	checkoutCmd := exec.Command("git", "-c", "core.autocrlf=false", "checkout-index", "-a", "-f")
	checkoutCmd.Dir = w.workspaceDir
	checkoutCmd.Env = env
	if err := checkoutCmd.Run(); err != nil {
		return fmt.Errorf("git checkout-index failed: %w", err)
	}

	return nil
}

// IsGitRepo reports whether workspaceDir is inside a valid Git working tree.
func (w *WorkspaceManager) IsGitRepo() bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = w.workspaceDir
	return cmd.Run() == nil
}
