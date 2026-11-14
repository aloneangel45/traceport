//go:build windows

package recorder

import "os/exec"

func resolveExitCode(exitErr *exec.ExitError, interrupted bool) int {
	if exitErr == nil {
		if interrupted {
			return 130
		}
		return 0
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
