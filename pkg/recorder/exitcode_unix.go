//go:build !windows

package recorder

import (
	"os/exec"
	"syscall"
)

func resolveExitCode(exitErr *exec.ExitError, interrupted bool) int {
	if exitErr == nil {
		if interrupted {
			return 130
		}
		return 0
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	code := exitErr.ExitCode()
	if code > 0 {
		return code
	}
	if interrupted {
		return 130
	}
	return 1
}
