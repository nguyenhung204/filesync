package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"filesync/internal/config"
)

// Conflicts + history/restore + UI availability edge cases over
// real daemons (job 006 acceptance list, second half).

func TestWebUIConflictsResolve(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	a := startDaemonCfg(t, "ui-gina", func(c *config.Config) { c.UIPort = freePort(t) })
	b := startDaemonCfg(t, "ui-hank", func(c *config.Config) { c.UIPort = freePort(t) })
	a.trustPeer(t, b)
	b.trustPeer(t, a)
	ua, ub := waitUI(t, a), waitUI(t, b)

	// Common base, synced to both.
	writeSyncFile(t, a.cfg.SyncDir, "shared.txt", "base content")
	waitFor(t, "base syncs", 15*time.Second, func() bool {
		return fileExists(b.cfg.SyncDir, "shared.txt")
	})

	// Both sides edit the same file while sync is paused: two
	// concurrent versions from one base = a deterministic conflict.
	if _, err := ua.post("/api/pause", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ub.post("/api/pause", nil); err != nil {
		t.Fatal(err)
	}
	writeSyncFile(t, a.cfg.SyncDir, "shared.txt", "edit from alice side")
	writeSyncFile(t, b.cfg.SyncDir, "shared.txt", "edit from bob side, different")
	// Let both scanners commit their local edits before resuming.
	time.Sleep(2 * time.Second)
	if _, err := ua.post("/api/resume", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ub.post("/api/resume", nil); err != nil {
		t.Fatal(err)
	}

	conflictFile := ""
	waitFor(t, "conflict copy appears on A", 20*time.Second, func() bool {
		matches, _ := filepath.Glob(filepath.Join(a.cfg.SyncDir, "shared (conflict *.txt"))
		if len(matches) == 1 {
			conflictFile = matches[0]
			return true
		}
		return false
	})
	list, err := ua.getList("/api/conflicts")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0]["original"] != "shared.txt" {
		t.Fatalf("conflicts = %v, want exactly one for shared.txt", list)
	}
	canonicalBefore, err := os.ReadFile(filepath.Join(a.cfg.SyncDir, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ua.post("/api/conflicts/resolve", map[string]any{
		"original": "shared.txt", "conflict": list[0]["conflict"], "keep": "mine",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := ua.getList("/api/conflicts")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("conflicts after resolve = %v, want empty", after)
	}
	canonicalAfter, err := os.ReadFile(filepath.Join(a.cfg.SyncDir, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(canonicalAfter) != string(canonicalBefore) {
		t.Fatalf("keep=mine changed canonical content: %q -> %q", canonicalBefore, canonicalAfter)
	}
	if fileExists(a.cfg.SyncDir, filepath.Base(conflictFile)) {
		t.Fatal("conflict copy still live after resolve")
	}
	// The loser survives in the trash (nothing was destroyed).
	trash, _ := filepath.Glob(filepath.Join(a.cfg.SyncDir, ".filesync", "trash", "*"))
	if len(trash) == 0 {
		t.Fatal("losing copy not preserved in .filesync/trash")
	}
}

func TestWebUIRestore(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	a := startDaemonCfg(t, "ui-ivan", func(c *config.Config) { c.UIPort = freePort(t) })
	b := startDaemonCfg(t, "ui-judy", func(c *config.Config) { c.UIPort = freePort(t) })
	a.trustPeer(t, b)
	b.trustPeer(t, a)
	ub := waitUI(t, b)

	writeSyncFile(t, a.cfg.SyncDir, "doc.txt", "version one")
	waitFor(t, "v1 reaches B", 15*time.Second, func() bool {
		raw, err := os.ReadFile(filepath.Join(b.cfg.SyncDir, "doc.txt"))
		return err == nil && string(raw) == "version one"
	})
	writeSyncFile(t, a.cfg.SyncDir, "doc.txt", "version two, longer")
	waitFor(t, "v2 reaches B (archiving v1)", 15*time.Second, func() bool {
		raw, err := os.ReadFile(filepath.Join(b.cfg.SyncDir, "doc.txt"))
		return err == nil && string(raw) == "version two, longer"
	})

	var versions []map[string]any
	waitFor(t, "B has an archived version", 15*time.Second, func() bool {
		var err error
		versions, err = ub.getList("/api/history?path=doc.txt")
		return err == nil && len(versions) >= 1
	})
	oldest := versions[len(versions)-1] // newest first
	if _, err := ub.post("/api/restore", map[string]any{"path": "doc.txt", "version": oldest["name"]}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(b.cfg.SyncDir, "doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "version one" {
		t.Fatalf("restored content = %q, want %q", raw, "version one")
	}
	// Undoable: the content the restore replaced ("version two")
	// is now the newest archived version.
	versions2, err := ub.getList("/api/history?path=doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions2) == 0 || versions2[0]["size"] != float64(len("version two, longer")) {
		t.Fatalf("newest version after restore = %v, want the replaced v2 on top", versions2)
	}
}

func TestWebUIDisabledDaemonUnaffected(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	port := freePort(t)
	td := startDaemonCfg(t, "ui-off", func(c *config.Config) {
		c.UIEnabled = false
		c.UIPort = port
	})
	if got := td.d.Status().UIAddr; got != "" {
		t.Fatalf("ui_addr = %q with UI disabled, want empty", got)
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("something is listening on the UI port with ui_enabled=false")
	}
	// Sync machinery is otherwise normal.
	if td.d.Status().State != "running" {
		t.Fatalf("state = %q, want running", td.d.Status().State)
	}
}

func TestWebUIPortBusyDaemonStillSyncs(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	port := freePort(t)
	a := startDaemonCfg(t, "ui-first", func(c *config.Config) { c.UIPort = port })
	waitUI(t, a) // first daemon owns the UI port now
	b := startDaemonCfg(t, "ui-second", func(c *config.Config) { c.UIPort = port })
	if got := b.d.Status().UIAddr; got != "" {
		t.Fatalf("second daemon ui_addr = %q, want empty (port busy)", got)
	}
	a.trustPeer(t, b)
	b.trustPeer(t, a)
	writeSyncFile(t, b.cfg.SyncDir, "busy-port.txt", "sync without ui")
	waitFor(t, "file syncs despite busy UI port", 15*time.Second, func() bool {
		return fileExists(a.cfg.SyncDir, "busy-port.txt")
	})
	raw, err := os.ReadFile(filepath.Join(b.cfg.ConfigDir, statusFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"state":"running"`) {
		t.Fatalf("second daemon status not running: %s", raw)
	}
}
