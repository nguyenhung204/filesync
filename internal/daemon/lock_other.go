//go:build !unix && !windows

package daemon

import "os"

// processAlive fallback: FindProcess success is treated as alive.
func processAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}
