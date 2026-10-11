package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"filesync/internal/config"
	"filesync/internal/history"
	"filesync/internal/store"
	"filesync/internal/validate"
)

// versionJSON is the --json shape of one archived version.
type versionJSON struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

// restoreJSON is the --json result of a successful restore.
type restoreJSON struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Size    int64  `json:"size"`
}

// openHistory opens the store and builds the history manager on
// it (the same construction cmdConflictsResolve uses). The caller
// closes the returned store.
func (e *env) openHistory() (config.Config, *store.Store, *history.Manager, error) {
	cfg, st, err := e.openStore()
	if err != nil {
		return cfg, nil, nil, err
	}
	hist := history.NewManager(cfg.SyncDir, st, cfg.VersionHistoryKeep, cfg.VersionHistoryQuotaBytes)
	return cfg, st, hist, nil
}

// cmdHistory lists the archived versions of one file, newest
// first. An empty history is not an error (exit 0, like
// `conflicts` with no conflicts).
func cmdHistory(e *env, args []string) int {
	var path string
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			return e.fail("history: unknown flag %q", a)
		}
		if path != "" {
			return e.fail("history: exactly one <path> expected")
		}
		path = a
	}
	if path == "" {
		return e.fail("usage: filesync history <path>")
	}
	cfg, st, hist, err := e.openHistory()
	if err != nil {
		return e.fail("history: %v", err)
	}
	defer st.Close()
	clean, err := validate.ValidateRelPath(path, cfg.MaxPathLength)
	if err != nil {
		return e.fail("history: %v", err)
	}
	versions, err := hist.ListVersions(clean)
	if err != nil {
		return e.fail("history: %v", err)
	}
	// The API returns oldest first; the CLI presents newest first.
	if e.json {
		out := []versionJSON{}
		for i := len(versions) - 1; i >= 0; i-- {
			v := versions[i]
			out = append(out, versionJSON{
				Name: v.Name, Path: v.Rel, Size: v.Size,
				ModTime: v.ModTime.Format(time.RFC3339),
			})
		}
		raw, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(e.stdout, string(raw))
		return 0
	}
	if len(versions) == 0 {
		fmt.Fprintf(e.stdout, "no versions for %s\n", clean)
		return 0
	}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		fmt.Fprintf(e.stdout, "%s  %d B  %s\n", v.Name, v.Size, v.ModTime.Format("2006-01-02 15:04:05"))
	}
	return 0
}

// cmdRestore puts one archived version of a file back at its
// original path. history.Restore archives the current content
// first, so a restore is itself undoable; the restored file is
// picked up by the watcher/rescan as a normal local edit.
func cmdRestore(e *env, args []string) int {
	var path, version string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--version":
			if i+1 >= len(args) {
				return e.fail("restore: --version needs a version name (see: filesync history <path>)")
			}
			version = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--version="):
			version = strings.TrimPrefix(args[i], "--version=")
		case strings.HasPrefix(args[i], "--"):
			return e.fail("restore: unknown flag %q", args[i])
		default:
			if path != "" {
				return e.fail("restore: exactly one <path> expected")
			}
			path = args[i]
		}
	}
	if path == "" {
		return e.fail("usage: filesync restore <path> [--version <name>]")
	}
	cfg, st, hist, err := e.openHistory()
	if err != nil {
		return e.fail("restore: %v", err)
	}
	defer st.Close()
	clean, err := validate.ValidateRelPath(path, cfg.MaxPathLength)
	if err != nil {
		return e.fail("restore: %v", err)
	}
	versions, err := hist.ListVersions(clean)
	if err != nil {
		return e.fail("restore: %v", err)
	}
	if len(versions) == 0 {
		return e.fail("restore: no versions for %s", clean)
	}
	target := versions[len(versions)-1] // newest
	if version != "" {
		found := false
		for _, v := range versions {
			if v.Name == version {
				target, found = v, true
				break
			}
		}
		if !found {
			names := make([]string, len(versions))
			for i, v := range versions {
				names[i] = v.Name
			}
			return e.fail("restore: version %q not found for %s; available versions:\n  %s",
				version, clean, strings.Join(names, "\n  "))
		}
	}
	if err := hist.Restore(clean, target.Name); err != nil {
		return e.fail("restore: %v", err)
	}
	if e.json {
		raw, _ := json.MarshalIndent(restoreJSON{Path: clean, Version: target.Name, Size: target.Size}, "", "  ")
		fmt.Fprintln(e.stdout, string(raw))
		return 0
	}
	fmt.Fprintf(e.stdout, "restored: %s from version %s\n", clean, target.Name)
	fmt.Fprintln(e.stdout, "the content it replaced was archived as the newest version — run restore again to undo")
	return 0
}
