package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// instanceLock is the single-instance guard (D7): an exclusively
// created file in the config dir carrying the holder's PID. The
// lock is advisory — it protects against accidental double-starts,
// not against a malicious local process.
type instanceLock struct {
	path string
}

type lockInfo struct {
	PID       int   `json:"pid"`
	StartedAt int64 `json:"started_at"` // UnixNano
}

func acquireLock(configDir string) (*instanceLock, error) {
	path := filepath.Join(configDir, "daemon.lock")
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			info := lockInfo{PID: os.Getpid(), StartedAt: time.Now().UnixNano()}
			raw, _ := json.Marshal(info)
			if _, werr := f.Write(raw); werr != nil {
				f.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("filesync: write daemon lock: %w", werr)
			}
			if cerr := f.Close(); cerr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("filesync: close daemon lock: %w", cerr)
			}
			return &instanceLock{path: path}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("filesync: create daemon lock in %s: %w", configDir, err)
		}
		// The lock file exists: a live holder is a hard error; a
		// dead (or unreadable/corrupt) one is stale and replaced.
		if pid, ok := readLockPID(path); ok && pid > 0 && processAlive(pid) {
			return nil, fmt.Errorf("filesync: daemon already running for config dir %s (pid %d)", configDir, pid)
		}
		_ = os.Remove(path)
	}
	return nil, fmt.Errorf("filesync: could not acquire daemon lock for config dir %s", configDir)
}

func readLockPID(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var info lockInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return 0, false
	}
	return info.PID, true
}

func (l *instanceLock) Release() {
	if l != nil && l.path != "" {
		_ = os.Remove(l.path)
	}
}
