package store

import (
	"os"
	"path/filepath"
	"testing"
)

// Round-2 regression (security NIT): the WAL sidecar holds the same
// index data as the DB itself, so Open must tighten it to 0600 too,
// not just filesync.db.
func TestRound2_WALSidecarPermissions(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	// Force at least one write so the WAL definitely exists.
	if err := st.JournalSet("k", []byte("v")); err != nil {
		t.Fatalf("JournalSet: %v", err)
	}
	wal := filepath.Join(dir, "filesync.db-wal")
	info, err := os.Stat(wal)
	if os.IsNotExist(err) {
		t.Skip("WAL sidecar not present on this platform/run")
	}
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("WAL sidecar permissions = %o, want 600", perm)
	}
}
