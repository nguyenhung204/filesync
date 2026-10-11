//go:build windows

package safefile

import (
	"fmt"
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// FreeSpace returns the number of bytes available to the calling user on
// the volume hosting path, via GetDiskFreeSpaceExW (stdlib only — no
// golang.org/x/sys dependency needed for this one call).
func FreeSpace(path string) (int64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("safefile: free space %q: %w", path, err)
	}
	var freeToCaller, total, totalFree uint64
	r, _, callErr := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, fmt.Errorf("safefile: free space %q: %w", path, callErr)
	}
	return int64(freeToCaller), nil
}
