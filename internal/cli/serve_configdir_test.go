package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The parser behind the v0.3.1 fix: service registrations put
// --config-dir after `serve`, and it must not be dropped.
func TestParseServeConfigDir(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{"no args", nil, "", false},
		{"space form", []string{"--config-dir", "/tmp/x"}, "/tmp/x", false},
		{"equals form", []string{"--config-dir=/tmp/x"}, "/tmp/x", false},
		{"after unrelated args", []string{"--verbose", "--config-dir", "/tmp/x"}, "/tmp/x", false},
		{"unrelated only", []string{"--verbose", "pos"}, "", false},
		{"missing value", []string{"--config-dir"}, "", true},
		{"empty space value", []string{"--config-dir", ""}, "", true},
		{"empty equals value", []string{"--config-dir="}, "", true},
		{"last wins", []string{"--config-dir", "/a", "--config-dir=/b"}, "/b", false},
	}
	for _, c := range cases {
		got, err := parseServeConfigDir(c.args)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: parseServeConfigDir(%v) = %q, want error", c.name, c.args, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: parseServeConfigDir(%v) error: %v", c.name, c.args, err)
		} else if got != c.want {
			t.Errorf("%s: parseServeConfigDir(%v) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}

// End-to-end regression for the Windows service bug: the service
// ImagePath is `"filesync.exe" serve --config-dir "<dir>"`. A
// config dir holding a corrupt config.json must produce a load
// error naming THAT dir's config file — proof the trailing flag
// took effect. Before the fix the flag was silently discarded,
// the daemon loaded the account-default config instead, and the
// error (or the daemon itself) pointed at the wrong directory.
func TestServeConfigDirAfterSubcommand(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "config.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	forms := map[string][]string{
		"space form (exact binPath shape)": {"serve", "--config-dir", dir},
		"equals form":                      {"serve", "--config-dir=" + dir},
		"global form (pre-existing)":       {"--config-dir", dir, "serve"},
	}
	for name, args := range forms {
		_, errs, rc := runCLI(t, "", args...)
		if rc == 0 {
			t.Errorf("%s: rc=0, want a config load failure naming %s", name, bad)
			continue
		}
		if !strings.Contains(errs, bad) {
			t.Errorf("%s: stderr = %q, want it to name %s", name, errs, bad)
		}
	}
	// A malformed trailing flag fails fast and loudly instead of
	// starting the daemon against the default config dir.
	_, errs, rc := runCLI(t, "", "serve", "--config-dir")
	if rc == 0 || !strings.Contains(errs, "--config-dir") {
		t.Fatalf("bare --config-dir: rc=%d stderr=%q, want a clear flag error", rc, errs)
	}
}

// Added by reviewer-test (round 1): build.md states the serve-level
// flag overrides the global one, but nothing pinned that down. Two
// corrupt config dirs make the winner observable without ever
// starting a daemon: the load error must name the serve-level dir.
func TestServeConfigDirOverridesGlobal(t *testing.T) {
	mkBad := func() string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	globalDir, serveDir := mkBad(), mkBad()
	forms := map[string][]string{
		"space form":  {"--config-dir", globalDir, "serve", "--config-dir", serveDir},
		"equals form": {"--config-dir", globalDir, "serve", "--config-dir=" + serveDir},
	}
	for name, args := range forms {
		_, errs, rc := runCLI(t, "", args...)
		want := filepath.Join(serveDir, "config.json")
		if rc == 0 || !strings.Contains(errs, want) {
			t.Errorf("%s: rc=%d stderr=%q, want a load failure naming %s (serve-level must win)",
				name, rc, errs, want)
		}
		if strings.Contains(errs, filepath.Join(globalDir, "config.json")) {
			t.Errorf("%s: stderr names the global dir %s — serve-level flag did not win", name, globalDir)
		}
	}
}
