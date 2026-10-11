package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tester round 1: quota eviction ORDER across several steps. The
// builder's test evicts exactly one version. Here the quota fits
// exactly 2 of four 8-byte versions archived in a known time order;
// after all four archives only the two NEWEST (c, d) may survive, in
// order, with their contents intact — evicting in any other order
// (newest-first, arbitrary) leaves a different surviving set.
func TestTester_QuotaEvictsInOldestFirstOrderAcrossFiles(t *testing.T) {
	m, root, _ := newManager(t)
	now, advance := testClock(time.Unix(1_700_000_000, 0))
	m.Now = now
	m.Keep = 100
	m.QuotaBytes = 16 // exactly two 8-byte versions fit

	names := []string{"a.txt", "b.txt", "c.txt", "d.txt"}
	for _, n := range names {
		writeFile(t, root, n, "12345678")
		if _, err := m.Archive(n); err != nil {
			t.Fatalf("Archive %s: %v", n, err)
		}
		advance(time.Second) // strictly increasing stamp names
	}

	wantSurvivors := map[string]bool{"a.txt": false, "b.txt": false, "c.txt": true, "d.txt": true}
	for _, n := range names {
		vs, err := m.ListVersions(n)
		if err != nil {
			t.Fatalf("ListVersions %s: %v", n, err)
		}
		if wantSurvivors[n] {
			if len(vs) != 1 {
				t.Fatalf("%s should survive quota, has %d versions", n, len(vs))
			}
			data, err := os.ReadFile(filepath.Join(root, ".filesync", "versions", n, vs[0].Name))
			if err != nil || string(data) != "12345678" {
				t.Fatalf("surviving version of %s damaged: %q, %v", n, data, err)
			}
		} else if len(vs) != 0 {
			t.Fatalf("%s should have been evicted oldest-first, still has %+v", n, vs)
		}
	}
}
