package cli

import (
	"fmt"
	"os"

	"filesync/internal/config"
	"filesync/internal/identity"
	"filesync/internal/store"
)

// cmdInit creates the config file, device identity, sync dir and
// store database. Running it again is a no-op that reprints the
// same device ID.
func cmdInit(e *env, _ []string) int {
	wasInitialized := e.initialized()
	if err := os.MkdirAll(e.configDir, 0o700); err != nil {
		return e.fail("init: create config dir: %v", err)
	}
	cfg, err := config.Load(e.configDir)
	if err != nil {
		return e.fail("init: %v", err)
	}
	if err := cfg.Save(); err != nil {
		return e.fail("init: save config: %v", err)
	}
	id, created, err := identity.LoadOrCreate(e.configDir)
	if err != nil {
		return e.fail("init: %v", err)
	}
	if err := os.MkdirAll(cfg.SyncDir, 0o755); err != nil {
		return e.fail("init: create sync dir: %v", err)
	}
	st, err := store.Open(e.configDir)
	if err != nil {
		return e.fail("init: open store: %v", err)
	}
	st.Close()

	if wasInitialized && !created {
		fmt.Fprintf(e.stdout, "already initialized: device %s\n", id.DeviceID)
	} else {
		fmt.Fprintf(e.stdout, "initialized: device %s\n", id.DeviceID)
	}
	fmt.Fprintf(e.stdout, "device_id:    %s\n", id.DeviceID)
	fmt.Fprintf(e.stdout, "fingerprint:  %s\n", id.Fingerprint)
	fmt.Fprintf(e.stdout, "config_dir:   %s\n", cfg.ConfigDir)
	fmt.Fprintf(e.stdout, "sync_dir:     %s\n", cfg.SyncDir)
	fmt.Fprintf(e.stdout, "listen_addr:  %s\n", cfg.ListenAddr)
	return 0
}
