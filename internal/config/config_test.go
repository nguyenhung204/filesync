package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfig_Defaults(t *testing.T) {
	c := Default()
	if c.ListenAddr != ":22000" {
		t.Errorf("ListenAddr = %q", c.ListenAddr)
	}
	if c.Parallelism != 4 {
		t.Errorf("Parallelism = %d, want 4", c.Parallelism)
	}
	if c.VersionHistoryKeep != 5 {
		t.Errorf("VersionHistoryKeep = %d, want 5", c.VersionHistoryKeep)
	}
	if c.VersionHistoryQuotaBytes != 1<<30 {
		t.Errorf("Quota = %d, want 1GiB", c.VersionHistoryQuotaBytes)
	}
	if DefaultMaxPathLength != 240 {
		t.Errorf("DefaultMaxPathLength = %d, want 240", DefaultMaxPathLength)
	}
	if c.MaxPathLength != DefaultMaxPathLength {
		t.Errorf("MaxPathLength = %d, want %d", c.MaxPathLength, DefaultMaxPathLength)
	}
	if c.BandwidthLimitKBps != 0 {
		t.Errorf("BandwidthLimitKBps = %d, want 0", c.BandwidthLimitKBps)
	}
	if c.DeviceName == "" || c.SyncDir == "" || c.ConfigDir == "" {
		t.Errorf("defaults must fill name/dirs: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}
}

func TestConfig_LoadMissingFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := Default()
	cfg.DeviceName, d.DeviceName = "", "" // host-dependent compare not needed, but keep others
	if cfg.ListenAddr != d.ListenAddr || cfg.Parallelism != d.Parallelism ||
		cfg.VersionHistoryKeep != d.VersionHistoryKeep || cfg.SyncDir != d.SyncDir {
		t.Fatalf("Load on empty dir = %+v, want defaults", cfg)
	}
	if cfg.ConfigDir != dir {
		t.Fatalf("ConfigDir = %q, want %q", cfg.ConfigDir, dir)
	}
}

func TestConfig_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.ConfigDir = dir
	cfg.DeviceName = "test-device"
	cfg.ListenAddr = "127.0.0.1:23001"
	cfg.Parallelism = 7
	cfg.BandwidthLimitKBps = 512
	cfg.MaxPathLength = 300
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ConfigDir") || strings.Contains(string(raw), "config_dir") {
		t.Fatalf("config.json must not contain ConfigDir: %s", raw)
	}
	back, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back != cfg {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", back, cfg)
	}
}

func TestConfig_LoadMergesOverDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"parallelism": 9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Parallelism != 9 {
		t.Fatalf("Parallelism = %d, want 9 from file", cfg.Parallelism)
	}
	if cfg.ListenAddr != ":22000" || cfg.VersionHistoryKeep != 5 {
		t.Fatalf("absent fields must keep defaults: %+v", cfg)
	}
}

func TestConfig_LoadRejectsCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	broken := []byte("{not json")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("corrupt JSON: expected error")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(broken) {
		t.Fatal("corrupt file was modified by Load")
	}
}

func TestConfig_ValidateTable(t *testing.T) {
	bad := []struct {
		name   string
		mutate func(*Config)
	}{
		{"port not a number", func(c *Config) { c.ListenAddr = ":http" }},
		{"no port", func(c *Config) { c.ListenAddr = "localhost" }},
		{"parallelism 0", func(c *Config) { c.Parallelism = 0 }},
		{"parallelism 33", func(c *Config) { c.Parallelism = 33 }},
		{"bandwidth negative", func(c *Config) { c.BandwidthLimitKBps = -1 }},
		{"quota negative", func(c *Config) { c.VersionHistoryQuotaBytes = -5 }},
		{"keep 0", func(c *Config) { c.VersionHistoryKeep = 0 }},
		{"keep 101", func(c *Config) { c.VersionHistoryKeep = 101 }},
		{"maxPathLength 8", func(c *Config) { c.MaxPathLength = 8 }},
		{"empty sync dir", func(c *Config) { c.SyncDir = "" }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.ConfigDir = t.TempDir()
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want error (%s)", tc.name)
			}
		})
	}
}

func TestConfig_SavePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions not applicable on Windows")
	}
	dir := filepath.Join(t.TempDir(), "cfg")
	cfg := Default()
	cfg.ConfigDir = dir
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm = %o, want 700", st.Mode().Perm())
	}
	st, err = os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config.json perm = %o, want 600", st.Mode().Perm())
	}
}

func TestConfig_SaveAtomicNoTempLeft(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.ConfigDir = dir
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	left, err := filepath.Glob(filepath.Join(dir, "config.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}
