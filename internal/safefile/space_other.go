//go:build !windows && !linux && !darwin

package safefile

import "errors"

// FreeSpace is unsupported on this platform; RequireSpace surfaces this
// error instead of guessing, so a receive is never silently waved through
// on a platform FileSync has not been ported to yet.
func FreeSpace(path string) (int64, error) {
	return 0, errors.New("safefile: free space check unsupported on this platform")
}
