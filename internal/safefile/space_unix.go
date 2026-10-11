//go:build linux || darwin

package safefile

import (
	"fmt"
	"syscall"
)

// FreeSpace returns the number of bytes available to an unprivileged user
// on the filesystem hosting path.
func FreeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("safefile: statfs %q: %w", path, err)
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
