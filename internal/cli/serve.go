package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"filesync/internal/daemon"
	"filesync/internal/logx"
)

// cmdServe runs the daemon in the foreground until SIGINT/SIGTERM.
// Logs (JSON) go to stderr; stdout stays clean.
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := d.Run(ctx); err != nil {
		// Includes the D7 "daemon already running" message, which
		// is printed verbatim (it is part of the CLI contract).
		return e.fail("%v", err)
	}
	return 0
}
