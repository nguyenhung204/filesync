//go:build unix

package daemon

import (
	"os"
	"syscall"
)

// processAlive on Unix: signal 0 probes existence without
// delivering a signal. EPERM still means the process exists.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}
