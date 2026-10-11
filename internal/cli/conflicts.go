package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"filesync/internal/history"
	"filesync/internal/identity"
	"filesync/internal/logx"
	fsync "filesync/internal/sync"
	"os"
)

// Conflict listing lives in sync.ListConflicts (job 006 D9f) — the
// single implementation shared with the web UI, built on
// sync.ParseConflictPath (the D3 naming rule) so neither consumer
// can drift from the engine's resolver.

// cmdConflicts lists conflict copies, or resolves one with the
// `resolve` subcommand.
func cmdConflicts(e *env, args []string) int {
	if len(args) > 0 && args[0] == "resolve" {
		return cmdConflictsResolve(e, args[1:])
	}
	_, st, err := e.openStore()
	if err != nil {
		return e.fail("conflicts: %v", err)
	}
	defer st.Close()
	entries, err := st.ListEntries()
	if err != nil {
		return e.fail("conflicts: %v", err)
	}
	infos := fsync.ListConflicts(entries)
	if e.json {
		if infos == nil {
			infos = []fsync.ConflictInfo{}
		}
		raw, _ := json.MarshalIndent(infos, "", "  ")
		fmt.Fprintln(e.stdout, string(raw))
		return 0
	}
	if len(infos) == 0 {
		fmt.Fprintln(e.stdout, "no conflicts")
		return 0
	}
	for _, ci := range infos {
		fmt.Fprintf(e.stdout, "%s\n  conflict copy: %s (loser %s…, %d B; original %d B)\n",
			ci.Original, ci.Conflict, ci.LoserDevice, ci.SizeConflict, ci.SizeOriginal)
	}
	fmt.Fprintln(e.stdout, "resolve with: filesync conflicts resolve <original> --keep mine|theirs")
	return 0
}

// cmdConflictsResolve runs sync.Engine.ResolveConflict with a
// minimal engine (no transport): the daemon, when running,
// reconciles on its next round (D6).
func cmdConflictsResolve(e *env, args []string) int {
	var original, keepStr, conflictPath string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--keep":
			if i+1 >= len(args) {
				return e.fail("resolve: --keep needs mine|theirs")
			}
			keepStr = args[i+1]
			i++
		case "--conflict":
			if i+1 >= len(args) {
				return e.fail("resolve: --conflict needs a path")
			}
			conflictPath = args[i+1]
			i++
		default:
			if strings.HasPrefix(args[i], "--") {
				return e.fail("resolve: unknown flag %q", args[i])
			}
			if original != "" {
				return e.fail("resolve: exactly one <original> path expected")
			}
			original = args[i]
		}
	}
	if original == "" {
		return e.fail("usage: filesync conflicts resolve <original> --keep mine|theirs [--conflict <path>]")
	}
	var keep fsync.KeepSide
	switch keepStr {
	case "mine":
		keep = fsync.KeepMine
	case "theirs":
		keep = fsync.KeepTheirs
	default:
		return e.fail("resolve: --keep is required and must be mine|theirs")
	}
	cfg, st, err := e.openStore()
	if err != nil {
		return e.fail("resolve: %v", err)
	}
	defer st.Close()
	entries, err := st.ListEntries()
	if err != nil {
		return e.fail("resolve: %v", err)
	}
	var copies []string
	for _, ci := range fsync.ListConflicts(entries) {
		if ci.Original == original {
			copies = append(copies, ci.Conflict)
		}
	}
	if conflictPath == "" {
		if len(copies) == 0 {
			return e.fail("resolve: no conflict copy found for %q", original)
		}
		if len(copies) > 1 {
			return e.fail("resolve: %d conflict copies exist for %q — pick one with --conflict:\n  %s",
				len(copies), original, strings.Join(copies, "\n  "))
		}
		conflictPath = copies[0]
	}
	id, _, err := identity.LoadOrCreate(cfg.ConfigDir)
	if err != nil {
		return e.fail("resolve: %v", err)
	}
	log := logx.New(os.Stderr, logx.LevelError)
	hist := history.NewManager(cfg.SyncDir, st, cfg.VersionHistoryKeep, cfg.VersionHistoryQuotaBytes)
	eng := fsync.NewEngine(cfg, id, st, hist, log, fsync.Options{})
	if err := eng.ResolveConflictCopy(original, conflictPath, keep); err != nil {
		return e.fail("resolve: %v", err)
	}
	keepWord := "your version"
	if keep == fsync.KeepTheirs {
		keepWord = "the conflict version"
	}
	fmt.Fprintf(e.stdout, "resolved: %s now holds %s\n", original, keepWord)
	fmt.Fprintln(e.stdout, "the losing version was preserved in .filesync/trash and .filesync/versions (nothing was destroyed)")
	return 0
}
