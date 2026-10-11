// Package cli implements the filesync command line. All logic
// lives here (not in main) so commands are testable in-process.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filesync/internal/config"
	"filesync/internal/store"
)

// Version is the build version, set by main from its ldflags var.
var Version = "dev"

// env carries the per-invocation streams and global flags.
type env struct {
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	configDir string
	json      bool
}

// Run is the process entry: it uses the real stdio streams.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithIO(args, os.Stdin, stdout, stderr)
}

// RunWithIO dispatches one command and returns the exit code.
func RunWithIO(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := &env{stdin: stdin, stdout: stdout, stderr: stderr}
	rest := args
	// Global flags precede the subcommand.
	for len(rest) > 0 {
		switch {
		case rest[0] == "--config-dir" && len(rest) > 1:
			e.configDir = rest[1]
			rest = rest[2:]
		case strings.HasPrefix(rest[0], "--config-dir="):
			e.configDir = strings.TrimPrefix(rest[0], "--config-dir=")
			rest = rest[1:]
		case rest[0] == "--json":
			e.json = true
			rest = rest[1:]
		default:
			goto parsed
		}
	}
parsed:
	if e.configDir == "" {
		e.configDir = config.Default().ConfigDir
	}
	if len(rest) == 0 {
		usage(e.stderr)
		return 2
	}
	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "init":
		return cmdInit(e, cmdArgs)
	case "serve":
		return cmdServe(e, cmdArgs)
	case "pair":
		return cmdPair(e, cmdArgs)
	case "devices":
		return cmdDevices(e, cmdArgs)
	case "status":
		return cmdStatus(e, cmdArgs)
	case "ui":
		return cmdUI(e, cmdArgs)
	case "uninstall-service":
		// Hidden helper invoked by the Windows Add/Remove
		// Programs entry (UninstallString); not listed in usage.
		return cmdUninstallService(e, cmdArgs)
	case "pause":
		return cmdPause(e, cmdArgs, true)
	case "resume":
		return cmdPause(e, cmdArgs, false)
	case "conflicts":
		return cmdConflicts(e, cmdArgs)
	case "history":
		return cmdHistory(e, cmdArgs)
	case "restore":
		return cmdRestore(e, cmdArgs)
	case "version":
		fmt.Fprintf(e.stdout, "filesync %s\n", Version)
		return 0
	case "help", "-h", "--help":
		usage(e.stdout)
		return 0
	default:
		fmt.Fprintf(e.stderr, "filesync: unknown command %q\n", cmd)
		usage(e.stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `filesync — two-way file sync without a cloud (phase 1, LAN)

Usage: filesync [--config-dir <dir>] [--json] <command> [args]

Commands:
  init                 create config, identity and sync dir (idempotent)
  serve                run the sync daemon (foreground)
  pair                 pair with another device (no args: show a code;
                       --to <addr> --code <6 digits>: dial a device)
  devices              list paired devices
  devices revoke <id>  revoke a device (full ID or unique prefix >= 8 chars)
  status               daemon status (live from status.json, else offline)
  ui                   open the web management UI in your browser
                       (daemon must be running; prints the URL)
  pause / resume       pause or resume syncing (works with daemon down)
  conflicts            list conflict copies
  conflicts resolve <path> --keep mine|theirs [--conflict <path>]
                       resolve a conflict (loser lands in trash/versions)
  history <path>       list archived versions of a file (newest first)
  restore <path> [--version <name>]
                       restore an archived version (default: newest);
                       the replaced content is archived first (undoable)
  version              print version
`)
}

// loadConfig loads the config for the env's config dir. A missing
// config file yields defaults (config.Load semantics).
func (e *env) loadConfig() (config.Config, error) {
	return config.Load(e.configDir)
}

// openStore loads config and opens the store in the config dir.
func (e *env) openStore() (config.Config, *store.Store, error) {
	cfg, err := e.loadConfig()
	if err != nil {
		return cfg, nil, err
	}
	st, err := store.Open(cfg.ConfigDir)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, st, nil
}

// initialized reports whether the config dir already holds an
// identity (i.e. `init` ran before).
func (e *env) initialized() bool {
	_, err := os.Stat(filepath.Join(e.configDir, "cert.pem"))
	return err == nil
}

// shortID renders the first 8 characters of a device ID.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (e *env) fail(format string, args ...any) int {
	fmt.Fprintf(e.stderr, "filesync: "+format+"\n", args...)
	return 1
}
