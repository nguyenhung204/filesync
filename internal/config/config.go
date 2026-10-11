// Package config loads and saves FileSync's JSON configuration, with
// defensive directory/file permissions and atomic writes.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// DefaultMaxPathLength is the default maximum sync path length in runes.
const DefaultMaxPathLength = 240

const fileName = "config.json"

// Config is the daemon configuration. ConfigDir is runtime-only and is never
// written to disk.
type Config struct {
	DeviceName               string `json:"device_name"`
	ListenAddr               string `json:"listen_addr"`
	SyncDir                  string `json:"sync_dir"`
	ConfigDir                string `json:"-"`
	BandwidthLimitKBps       int    `json:"bandwidth_limit_kbps"` // 0 = unlimited
	Parallelism              int    `json:"parallelism"`
	VersionHistoryKeep       int    `json:"version_history_keep"`
	VersionHistoryQuotaBytes int64  `json:"version_history_quota_bytes"` // 0 = unlimited
	MaxPathLength            int    `json:"max_path_length"`
	// RescanIntervalSeconds is the periodic full-rescan interval the
	// daemon hands to the watcher (job 004 spec D5: the number is 30s
	// everywhere — code, config default and README). Additive field:
	// config files written before it existed load fine; 0 means "use
	// the default of 30" when the daemon reads it.
	RescanIntervalSeconds int `json:"rescan_interval_seconds"`
}

func defaultConfigDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "filesync")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "filesync")
	}
	return "./.filesync"
}

func defaultSyncDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "FileSync")
	}
	return "./FileSync"
}

// Default returns the default configuration.
func Default() Config {
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "filesync-device"
	}
	return Config{
		DeviceName:               name,
		ListenAddr:               ":22000",
		SyncDir:                  defaultSyncDir(),
		ConfigDir:                defaultConfigDir(),
		BandwidthLimitKBps:       0,
		Parallelism:              4,
		VersionHistoryKeep:       5,
		VersionHistoryQuotaBytes: 1 << 30,
		MaxPathLength:            DefaultMaxPathLength,
		RescanIntervalSeconds:    30,
	}
}

// Load reads <configDir>/config.json over the defaults: fields absent from
// the JSON keep their defaults, unknown fields are ignored, and a missing
// file returns the defaults with a nil error. A corrupt file or invalid
// values return a wrapped error and leave the file untouched.
func Load(configDir string) (Config, error) {
	if configDir == "" {
		// An empty dir would make filepath.Join read ./config.json from
		// the working directory — refuse instead of guessing.
		return Config{}, fmt.Errorf("config: load: config_dir must not be empty")
	}
	cfg := Default()
	cfg.ConfigDir = configDir
	path := filepath.Join(configDir, fileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := cfg.Validate(); err != nil {
				return cfg, fmt.Errorf("config: load %s: %w", path, err)
			}
			return cfg, nil
		}
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.ConfigDir = configDir
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("config: load %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks field values, returning an error naming the first bad field.
func (c Config) Validate() error {
	_, portStr, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("config: listen_addr %q: %w", c.ListenAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("config: listen_addr %q: port %q is not a number in 0-65535", c.ListenAddr, portStr)
	}
	if c.SyncDir == "" {
		return fmt.Errorf("config: sync_dir must not be empty")
	}
	if c.ConfigDir == "" {
		return fmt.Errorf("config: config_dir must not be empty")
	}
	if c.Parallelism < 1 || c.Parallelism > 32 {
		return fmt.Errorf("config: parallelism %d out of range 1-32", c.Parallelism)
	}
	if c.BandwidthLimitKBps < 0 {
		return fmt.Errorf("config: bandwidth_limit_kbps %d must be >= 0", c.BandwidthLimitKBps)
	}
	if c.VersionHistoryQuotaBytes < 0 {
		return fmt.Errorf("config: version_history_quota_bytes %d must be >= 0", c.VersionHistoryQuotaBytes)
	}
	if c.VersionHistoryKeep < 1 || c.VersionHistoryKeep > 100 {
		return fmt.Errorf("config: version_history_keep %d out of range 1-100", c.VersionHistoryKeep)
	}
	if c.MaxPathLength < 16 || c.MaxPathLength > 4096 {
		return fmt.Errorf("config: max_path_length %d out of range 16-4096", c.MaxPathLength)
	}
	if c.RescanIntervalSeconds < 0 {
		return fmt.Errorf("config: rescan_interval_seconds %d must be >= 0", c.RescanIntervalSeconds)
	}
	return nil
}

// ensureDir creates dir with 0700 permissions and tightens an existing dir
// to 0700. NOTE: config and identity each carry this small helper on purpose:
// the two packages must stay independent so later jobs can use either alone.
// On Windows Chmod has near-no-op semantics; there we rely on the user
// profile directory's ACLs.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod dir %s: %w", dir, err)
	}
	return nil
}

func fsyncDir(dir string) {
	// Best-effort: directory fsync is unsupported on some platforms.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// Save writes the config atomically to <ConfigDir>/config.json: temp file in
// the same directory with 0600 permissions, fsync, rename over the target.
func (c Config) Save() error {
	if c.ConfigDir == "" {
		return fmt.Errorf("config: save: config_dir is empty")
	}
	if err := ensureDir(c.ConfigDir); err != nil {
		return fmt.Errorf("config: save: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(c.ConfigDir, fileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("config: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("config: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("config: fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close temp file: %w", err)
	}
	target := filepath.Join(c.ConfigDir, fileName)
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("config: rename temp file to %s: %w", target, err)
	}
	committed = true
	fsyncDir(c.ConfigDir)
	return nil
}
