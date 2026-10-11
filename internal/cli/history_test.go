package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesync/internal/config"
	"filesync/internal/history"
	"filesync/internal/store"
)

// seedHistory archives each of archived (in order) as a version of
// rel, then leaves live as the current file content — or deletes
// the file when live is "". Returns the versions, oldest first.
// The config dir must already be initialized.
func seedHistory(t *testing.T, dir, rel, live string, archived ...string) []history.VersionInfo {
	t.Helper()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hist := history.NewManager(cfg.SyncDir, st, cfg.VersionHistoryKeep, cfg.VersionHistoryQuotaBytes)
	abs := filepath.Join(cfg.SyncDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range archived {
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := hist.Archive(rel); err != nil {
			t.Fatal(err)
		}
	}
	switch {
	case live != "":
		if err := os.WriteFile(abs, []byte(live), 0o644); err != nil {
			t.Fatal(err)
		}
	case len(archived) > 0:
		if err := os.Remove(abs); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := hist.ListVersions(rel)
	if err != nil {
		t.Fatal(err)
	}
	return versions
}

func readSyncFile(t *testing.T, dir, rel string) string {
	t.Helper()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.SyncDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCLIHistoryEmpty(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	out, _, rc := runCLI(t, "", "--config-dir", dir, "history", "ghost.txt")
	if rc != 0 || !strings.Contains(out, "no versions for ghost.txt") {
		t.Fatalf("history empty out=%q rc=%d", out, rc)
	}
	out, _, rc = runCLI(t, "", "--config-dir", dir, "--json", "history", "ghost.txt")
	if rc != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("history empty --json out=%q rc=%d, want []", out, rc)
	}
}

func TestCLIHistoryListNewestFirstAndJSON(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	versions := seedHistory(t, dir, "doc.txt", "live-content", "first-version", "second-version!")
	if len(versions) != 2 {
		t.Fatalf("seeded %d versions, want 2", len(versions))
	}
	out, _, rc := runCLI(t, "", "--config-dir", dir, "history", "doc.txt")
	if rc != 0 {
		t.Fatalf("history rc=%d out=%q", rc, out)
	}
	oldIdx, newIdx := strings.Index(out, versions[0].Name), strings.Index(out, versions[1].Name)
	if oldIdx < 0 || newIdx < 0 {
		t.Fatalf("history output missing a version name: %q (versions %+v)", out, versions)
	}
	if newIdx > oldIdx {
		t.Fatalf("history not newest-first: %q", out)
	}
	if !strings.Contains(out, "15 B") { // len("second-version!")
		t.Fatalf("history output missing size: %q", out)
	}
	out, _, rc = runCLI(t, "", "--config-dir", dir, "--json", "history", "doc.txt")
	if rc != 0 {
		t.Fatalf("history --json rc=%d", rc)
	}
	var got []versionJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("history --json unparsable: %v (%q)", err, out)
	}
	if len(got) != 2 || got[0].Name != versions[1].Name || got[1].Name != versions[0].Name {
		t.Fatalf("history --json = %+v, want newest first of %+v", got, versions)
	}
	if got[0].Path != "doc.txt" || got[0].Size != versions[1].Size {
		t.Fatalf("history --json fields wrong: %+v", got[0])
	}
	if _, err := time.Parse(time.RFC3339, got[0].ModTime); err != nil {
		t.Fatalf("mod_time %q not RFC 3339: %v", got[0].ModTime, err)
	}
}

func TestCLIRestoreDefaultAndUndo(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	seedHistory(t, dir, "doc.txt", "current-live", "old-one", "old-two")
	out, _, rc := runCLI(t, "", "--config-dir", dir, "restore", "doc.txt")
	if rc != 0 || !strings.Contains(out, "restored: doc.txt from version") {
		t.Fatalf("restore out=%q rc=%d", out, rc)
	}
	if got := readSyncFile(t, dir, "doc.txt"); got != "old-two" {
		t.Fatalf("after restore file = %q, want newest version %q", got, "old-two")
	}
	// The replaced content was archived: restore again undoes.
	out, _, rc = runCLI(t, "", "--config-dir", dir, "history", "doc.txt")
	if rc != 0 || strings.Count(strings.TrimSpace(out), "\n")+1 != 3 {
		t.Fatalf("history after restore = %q rc=%d, want 3 versions", out, rc)
	}
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "restore", "doc.txt"); rc != 0 {
		t.Fatal("undo restore failed")
	}
	if got := readSyncFile(t, dir, "doc.txt"); got != "current-live" {
		t.Fatalf("after undo file = %q, want %q", got, "current-live")
	}
}

func TestCLIRestoreSpecificVersionAndJSON(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	versions := seedHistory(t, dir, "doc.txt", "current-live", "old-one", "old-two")
	out, _, rc := runCLI(t, "", "--config-dir", dir, "--json", "restore", "doc.txt", "--version", versions[0].Name)
	if rc != 0 {
		t.Fatalf("restore --version rc=%d out=%q", rc, out)
	}
	var res restoreJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("restore --json unparsable: %v (%q)", err, out)
	}
	if res.Path != "doc.txt" || res.Version != versions[0].Name || res.Size != versions[0].Size {
		t.Fatalf("restore --json = %+v", res)
	}
	if got := readSyncFile(t, dir, "doc.txt"); got != "old-one" {
		t.Fatalf("after restore --version file = %q, want %q", got, "old-one")
	}
}

func TestCLIRestoreUnknownVersion(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	versions := seedHistory(t, dir, "doc.txt", "current-live", "old-one")
	_, errs, rc := runCLI(t, "", "--config-dir", dir, "restore", "doc.txt", "--version", "no-such-version")
	if rc != 1 {
		t.Fatalf("restore unknown version rc=%d, want 1", rc)
	}
	if !strings.Contains(errs, versions[0].Name) {
		t.Fatalf("error should list available versions, errs=%q", errs)
	}
	if got := readSyncFile(t, dir, "doc.txt"); got != "current-live" {
		t.Fatalf("live file changed by failed restore: %q", got)
	}
	out, _, _ := runCLI(t, "", "--config-dir", dir, "history", "doc.txt")
	if strings.Count(strings.TrimSpace(out), "\n")+1 != 1 {
		t.Fatalf("version list changed by failed restore: %q", out)
	}
}

func TestCLIRestoreNoVersions(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	_, errs, rc := runCLI(t, "", "--config-dir", dir, "restore", "ghost.txt")
	if rc != 1 || !strings.Contains(errs, "no versions") {
		t.Fatalf("restore with no versions rc=%d errs=%q", rc, errs)
	}
}

func TestCLIRestoreDeletedLiveFile(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	seedHistory(t, dir, "doc.txt", "", "only-version")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "restore", "doc.txt"); rc != 0 {
		t.Fatal("restore of deleted file failed")
	}
	if got := readSyncFile(t, dir, "doc.txt"); got != "only-version" {
		t.Fatalf("revived file = %q, want %q", got, "only-version")
	}
}

func TestCLIRestoreDestinationBlocked(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	seedHistory(t, dir, "doc.txt", "current-live", "old-one")
	// Replace the live file with a directory: the destination can
	// no longer be written atomically, whatever the privileges.
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(cfg.SyncDir, "doc.txt")
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, rc := runCLI(t, "", "--config-dir", dir, "restore", "doc.txt")
	if rc != 1 {
		t.Fatalf("restore onto a directory rc=%d, want 1", rc)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		t.Fatalf("destination changed by failed restore: %v", err)
	}
	out, _, _ := runCLI(t, "", "--config-dir", dir, "history", "doc.txt")
	if strings.Count(strings.TrimSpace(out), "\n")+1 != 1 {
		t.Fatalf("version list changed by failed restore: %q", out)
	}
}

func TestCLIHistoryRestoreBadPaths(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	for _, bad := range []string{"../escape.txt", "/etc/passwd", "a/../../b.txt"} {
		if _, _, rc := runCLI(t, "", "--config-dir", dir, "history", bad); rc != 1 {
			t.Fatalf("history %q rc=%d, want 1", bad, rc)
		}
		if _, _, rc := runCLI(t, "", "--config-dir", dir, "restore", bad); rc != 1 {
			t.Fatalf("restore %q rc=%d, want 1", bad, rc)
		}
	}
	// Argument-shape errors also fail with exit 1.
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "history"); rc != 1 {
		t.Fatal("history without a path succeeded")
	}
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "history", "a.txt", "b.txt"); rc != 1 {
		t.Fatal("history with two paths succeeded")
	}
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "history", "--bogus", "a.txt"); rc != 1 {
		t.Fatal("history with unknown flag succeeded")
	}
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "restore", "a.txt", "--version"); rc != 1 {
		t.Fatal("restore with dangling --version succeeded")
	}
}
