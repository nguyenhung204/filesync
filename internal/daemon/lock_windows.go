//go:build windows

package daemon

import "os"

// processAlive on Windows: os.FindProcess opens a handle to the
// process; success means it exists. This cannot distinguish PID
// reuse (a documented phase-1 limitation).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
