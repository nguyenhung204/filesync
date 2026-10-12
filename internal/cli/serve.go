package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"filesync/internal/daemon"
	"filesync/internal/logx"
)

// cmdServe runs the daemon until told to stop. On Windows it
// first tries to run as a real SCM service (serve_windows.go);
// everywhere else — and in a Windows console — it runs in the
// foreground until SIGINT/SIGTERM. Logs (JSON) go to stderr;
// stdout stays clean.
func cmdServe(e *env, _ []string) int {
	cfg, err := e.loadConfig()
	if err != nil {
		return e.fail("serve: %v", err)
	}
	log := logx.New(e.stderr, logx.LevelInfo)
	d, err := daemon.New(cfg, log)
	if err != nil {
		return e.fail("serve: %v", err)
	}
	return runServe(e, d, log)
}

// runConsoleServe is the historical foreground behaviour: block
// until SIGINT/SIGTERM, then let the daemon shut down gracefully.
func runConsoleServe(e *env, d *daemon.Daemon) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := d.Run(ctx); err != nil {
		// Includes the D7 "daemon already running" message, which
		// is printed verbatim (it is part of the CLI contract).
		return e.fail("%v", err)
	}
	return 0
}
