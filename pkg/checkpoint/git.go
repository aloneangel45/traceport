package checkpoint

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

type WorkspaceManager struct {
	workspaceDir string
}

func NewWorkspaceManager(dir string) *WorkspaceManager {
	return &WorkspaceManager{workspaceDir: dir}
}

// CreateSnapshot creates an instantaneous git write-tree snapshot without creating a visible commit
func (w *WorkspaceManager) CreateSnapshot() (string, error) {
	// First add changes to index temporarily
	addCmd := exec.Command("git", "add", "-A")
	addCmd.Dir = w.workspaceDir
	if err := addCmd.Run(); err != nil {
		return "", fmt.Errorf("git add failed: %w", err)
	}

	// Write tree returns a unique 40-character SHA hash of the full working directory
	treeCmd := exec.Command("git", "write-tree")
	treeCmd.Dir = w.workspaceDir
	var out bytes.Buffer
	treeCmd.Stdout = &out

	if err := treeCmd.Run(); err != nil {
		return "", fmt.Errorf("git write-tree failed: %w", err)
	}

	treeHash := strings.TrimSpace(out.String())
	return treeHash, nil
}

// RestoreSnapshot checks out the workspace files to the exact state saved in treeHash
func (w *WorkspaceManager) RestoreSnapshot(treeHash string) error {
	if treeHash == "" {
		return fmt.Errorf("empty tree hash")
	}

	// Read-tree resets the index to the snapshot
	readCmd := exec.Command("git", "read-tree", treeHash)
	readCmd.Dir = w.workspaceDir
	if err := readCmd.Run(); err != nil {
		return fmt.Errorf("git read-tree failed: %w", err)
	}

	// Checkout-index writes the files back to the working tree
	checkoutCmd := exec.Command("git", "checkout-index", "-a", "-f")
	checkoutCmd.Dir = w.workspaceDir
	if err := checkoutCmd.Run(); err != nil {
		return fmt.Errorf("git checkout-index failed: %w", err)
	}

	return nil
}

func (w *WorkspaceManager) IsGitRepo() bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = w.workspaceDir
	return cmd.Run() == nil
}
