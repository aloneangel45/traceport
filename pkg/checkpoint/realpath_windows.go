//go:build windows

package checkpoint

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	modKernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetFinalPathNameByHandle = modKernel32.NewProc("GetFinalPathNameByHandleW")
)

func evalPlatformRealPath(abs string) (string, error) {
	pathPtr, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		return filepath.EvalSymlinks(abs)
	}
	handle, err := syscall.CreateFile(
		pathPtr,
		0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return filepath.EvalSymlinks(abs)
	}
	defer syscall.CloseHandle(handle)

	buf := make([]uint16, 512)
	for {
		n, _, callErr := procGetFinalPathNameByHandle.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
			0,
		)
		if n == 0 {
			if eval, eErr := filepath.EvalSymlinks(abs); eErr == nil {
				return eval, nil
			}
			return "", callErr
		}
		if int(n) < len(buf) {
			resolved := syscall.UTF16ToString(buf[:n])
			if strings.HasPrefix(resolved, `\\?\UNC\`) {
				resolved = `\\` + resolved[len(`\\?\UNC\`):]
			} else if strings.HasPrefix(resolved, `\\?\`) && len(resolved) >= 6 && resolved[5] == ':' {
				resolved = resolved[len(`\\?\`):]
			}
			return filepath.Clean(resolved), nil
		}
		buf = make([]uint16, int(n)+1)
	}
}

func isPlatformLinkMode(mode fs.FileMode) bool {
	return mode&(os.ModeSymlink|os.ModeIrregular) != 0
}
