package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Job 006: the UI fields are additive — an old config file without
// them loads with the defaults filled in.
func TestUIConfigDefaultsAndLegacyLoad(t *testing.T) {
	d := Default()
	if !d.UIEnabled {
		t.Fatal("Default().UIEnabled = false, want true")
	}
	if d.UIPort != DefaultUIPort || d.EffectiveUIPort() != DefaultUIPort {
		t.Fatalf("Default().UIPort = %d, want %d", d.UIPort, DefaultUIPort)
	}

	dir := t.TempDir()
	legacy := `{"device_name":"old","listen_addr":":22000","sync_dir":"/tmp/sync"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UIEnabled || cfg.UIPort != DefaultUIPort {
		t.Fatalf("legacy load: ui_enabled=%v ui_port=%d, want true/%d", cfg.UIEnabled, cfg.UIPort, DefaultUIPort)
	}
}

func TestUIConfigPortZeroMeansDefault(t *testing.T) {
	cfg := Default()
	cfg.UIPort = 0
	if cfg.EffectiveUIPort() != DefaultUIPort {
		t.Fatalf("EffectiveUIPort = %d, want %d", cfg.EffectiveUIPort(), DefaultUIPort)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ui_port 0 must validate: %v", err)
	}
	cfg.UIPort = 65536
	if err := cfg.Validate(); err == nil {
		t.Fatal("ui_port 65536 must not validate")
	}
	cfg.UIPort = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("ui_port -1 must not validate")
	}
}
