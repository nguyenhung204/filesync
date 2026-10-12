//go:build !windows

package cli

import (
	"filesync/internal/daemon"
	"filesync/internal/logx"
)

// runServe on non-Windows platforms is always the console path:
// there is no Service Control Manager to connect to.
func runServe(e *env, d *daemon.Daemon, _ *logx.Logger) int {
	return runConsoleServe(e, d)
}
