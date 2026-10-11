package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tester round 2 — quota variant of code BLOCKER C1 (round 1): restoring
// the OLDEST version while the versions tree is at the quota limit. The
// archive of the live file inside Restore triggers enforceQuota, whose
// oldest-first eviction candidate is the restore target itself; without
// target protection the target is evicted and Restore fails. The round-2
// regression test only covers the keep-limit (prune) path, not quota.
func TestTester_RestoreOldestVersionAtQuotaLimit(t *testing.T) {
	m, root, _ := newManager(t)
	now, advance := testClock(time.Unix(1_700_000_000, 0))
	m.Now = now
	m.QuotaBytes = 0 // unlimited during setup; tightened before restore

	writeFile(t, root, "doc.txt", "AAAA") // restore target, oldest in tree
	if _, err := m.Archive("doc.txt"); err != nil {
		t.Fatalf("Archive target: %v", err)
	}
	advance(time.Second)
	writeFile(t, root, "doc.txt", "BBBB")
	if _, err := m.Archive("doc.txt"); err != nil {
		t.Fatalf("Archive second version: %v", err)
	}
	advance(time.Second)
	writeFile(t, root, "other.txt", "CCCCCCCC")
	if _, err := m.Archive("other.txt"); err != nil {
		t.Fatalf("Archive other file: %v", err)
	}
	// Tree now holds 4 + 4 + 8 = 16 bytes of versions.

	versions, err := m.ListVersions("doc.txt")
	if err != nil || len(versions) != 2 {
		t.Fatalf("doc versions = %d, %v; want 2", len(versions), err)
	}
	target := versions[0]

	// Tighten the quota to exactly the current tree size. Restoring
	// archives the live file (4 more bytes), so quota enforcement must
	// evict 4 bytes — the oldest UNPROTECTED version (BBBB).
	m.QuotaBytes = 16
	writeFile(t, root, "doc.txt", "DDDD")
	if err := m.Restore("doc.txt", target.Name); err != nil {
		t.Fatalf("Restore oldest at quota limit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "AAAA" {
		t.Fatalf("restored content = %q, want AAAA", data)
	}

	versions, err = m.ListVersions("doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	var sawTarget, sawLive bool
	var total int64
	for _, v := range versions {
		total += v.Size
		if v.Name == target.Name {
			sawTarget = true
		}
		b, err := os.ReadFile(filepath.Join(root, MetaDir, "versions", "doc.txt", v.Name))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) == "BBBB" {
			t.Fatal("BBBB version survived; quota eviction should have taken it first")
		}
		if string(b) == "DDDD" {
			sawLive = true
		}
	}
	others, err := m.ListVersions("other.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range others {
		total += v.Size
	}
	if !sawTarget {
		t.Fatal("restore target version was evicted by quota enforcement")
	}
	if !sawLive {
		t.Fatal("pre-restore live content was not archived")
	}
	if total > 16 {
		t.Fatalf("versions tree = %d bytes, want <= quota 16", total)
	}
}
