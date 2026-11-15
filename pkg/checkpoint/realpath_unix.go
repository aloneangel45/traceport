//go:build !windows

package checkpoint

import (
	"io/fs"
	"os"
	"path/filepath"
)

func evalPlatformRealPath(abs string) (string, error) {
	return filepath.EvalSymlinks(abs)
}

func isPlatformLinkMode(mode fs.FileMode) bool {
	return mode&os.ModeSymlink != 0
}
