package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Job 002c, mục 6 (NIT): Load("") must always error — it must never fall
// back to reading ./config.json from the working directory. The
// no-config.json subtest is the one that failed before the fix: the old
// missing-file branch returned the defaults with a nil error without
// ever running Validate, even though ConfigDir was empty.
func TestConfig_LoadEmptyConfigDir(t *testing.T) {
	t.Run("no config.json in working directory", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if _, err := Load(""); err == nil {
			t.Fatal(`Load(""): expected error, got nil`)
		}
	})
	t.Run("config.json present in working directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, fileName), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if _, err := Load(""); err == nil {
			t.Fatal(`Load(""): expected error, got nil`)
		}
	})
}

// Job 002c, mục 6 (NIT): the missing-file branch returns validated
// defaults (Load("") failing in the test above proves Validate now runs
// on that branch, since the defaults carry an empty ConfigDir there).
func TestConfig_LoadMissingFileStillValidates(t *testing.T) {
	dir := t.TempDir() // no config.json in here
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(missing file): err = %v, want nil", err)
	}
	if cfg.ConfigDir != dir {
		t.Errorf("ConfigDir = %q, want %q", cfg.ConfigDir, dir)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("returned defaults fail Validate: %v", err)
	}
}
