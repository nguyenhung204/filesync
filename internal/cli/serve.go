package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"filesync/internal/daemon"
	"filesync/internal/logx"
)

// parseServeConfigDir extracts a --config-dir flag from serve's
// own arguments. Windows service registrations invoke the daemon
// as `filesync.exe serve --config-dir <dir>` — the flag AFTER the
// subcommand (see binPathValue in cmd/filesync-setup) — while the
// global parser in cli.go only consumes flags BEFORE the
// subcommand. Honouring the flag here also heals registrations
// created by older installers: they start passing the right
// config dir to the daemon as soon as the binary is upgraded, no
// re-registration needed. Last occurrence wins, matching the
// global parser; a serve-level flag also overrides a global one.
// A --config-dir with a missing or empty value is an error
// rather than silently ignored: silently falling back to the
// account-default config dir is exactly how a service ended up
// running under the systemprofile config dir on Windows. Other
// arguments stay ignored, as before.
func parseServeConfigDir(args []string) (string, error) {
	dir := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config-dir":
			if i+1 >= len(args) || args[i+1] == "" {
				return "", fmt.Errorf("--config-dir requires a value")
			}
			dir = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config-dir="):
			v := strings.TrimPrefix(args[i], "--config-dir=")
			if v == "" {
				return "", fmt.Errorf("--config-dir requires a value")
			}
			dir = v
		}
	}
	return dir, nil
}

// cmdServe runs the daemon until told to stop. On Windows it
// first tries to run as a real SCM service (serve_windows.go);
// everywhere else — and in a Windows console — it runs in the
// foreground until SIGINT/SIGTERM. Logs (JSON) go to stderr;
// stdout stays clean.
func cmdServe(e *env, args []string) int {
	if dir, err := parseServeConfigDir(args); err != nil {
		return e.fail("serve: %v", err)
	} else if dir != "" {
		e.configDir = dir
	}
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
