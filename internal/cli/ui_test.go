package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filesync/internal/config"
	"filesync/internal/webui"
)

// `filesync ui` without a daemon: no token file exists, so it must
// fail with the spec's message and never create a token.
func TestUICommandNoDaemon(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	_, errs, rc := runCLI(t, "", "--config-dir", dir, "ui")
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 (stderr: %s)", rc, errs)
	}
	if !strings.Contains(errs, "web UI token not found") {
		t.Fatalf("stderr = %q, want token-not-found message", errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "ui-token")); !os.IsNotExist(err) {
		t.Fatal("ui command created a token file")
	}
}

// `filesync ui` with ui_enabled=false fails before touching tokens.
func TestUICommandDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ConfigDir = dir
	cfg.SyncDir = filepath.Join(dir, "sync")
	cfg.UIEnabled = false
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	_, errs, rc := runCLI(t, "", "--config-dir", dir, "ui")
	if rc != 1 || !strings.Contains(errs, "web UI is disabled") {
		t.Fatalf("rc = %d stderr = %q, want disabled message", rc, errs)
	}
}

// With a token file present, the command prints the URL (with the
// default port when no live status exists) and exits 0 even if no
// browser can be opened here.
func TestUICommandPrintsURL(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	token, err := webui.LoadOrCreateToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, _, rc := runCLI(t, "", "--config-dir", dir, "ui")
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (out: %s)", rc, out)
	}
	want := "http://127.0.0.1:22080/?token=" + token
	if !strings.Contains(out, want) {
		t.Fatalf("out = %q, want URL %q", out, want)
	}
	if !strings.Contains(out, "do not share") {
		t.Fatalf("out missing token warning: %q", out)
	}
}
