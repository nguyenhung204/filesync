//go:build e2e

package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"filesync/internal/store"
)

// TestE2EResume20GiB is the job-005 heavyweight check (Q2-A),
// answering "can FileSync move a 20 GiB file?" with real numbers:
// a sparse 20 GiB file with 64 deterministic incompressible 1 MiB
// pattern blocks is pulled A→B; B is SIGKILLed past the halfway
// point; after restart B must receive ONLY the missing remainder —
// leg-2 bytes_received (fresh process counters) must be > 0 and
// < 60% of 20 GiB — and the final file must hash-identical to the
// source.
//
// Gated twice so it never runs by accident: the e2e build tag AND
// FILESYNC_TEST_20GIB=1. Standard run:
//
//	FILESYNC_TEST_20GIB=1 go test -tags=e2e ./internal/e2e/ -run TestE2EResume20GiB -timeout 60m -v
//
// Note on pacing: unlike the incompressible 1 GiB test, the wire
// here is tiny next to the raw size (the sparse holes gzip to
// ~0.1%), so the bandwidth throttle cannot pace leg 1 by itself;
// the kill is driven by polling B's status BytesReceived, which
// counts RAW chunk bytes. Note on hashing: the shared fileHash
// helper reads whole files into RAM — at 20 GiB that would OOM a
// 7.9 GiB machine, so this test hashes by streaming (streamHash).
func TestE2EResume20GiB(t *testing.T) {
	if os.Getenv("FILESYNC_TEST_20GIB") != "1" {
		t.Skip("set FILESYNC_TEST_20GIB=1 to run the 20 GiB resume test")
	}
	// Disk precondition on the filesystem that will actually hold
	// the node dirs (testBase root): peak usage is B's staging +
	// final ≈ 20 GiB (the source is sparse), budgeted at 50 GiB
	// with headroom. /tmp is a 512 MiB tmpfs on this machine and
	// must never hold these nodes.
	root := e2eRoot(t)
	const needFree = 50 << 30
	if free := freeBytesAt(t, root); free < needFree {
		t.Skipf("need >= %d GiB free on %s, have %d GiB", needFree>>30, root, free>>30)
	}

	const size = 20 << 30
	a := newNode(t, "huge-a")
	b := newNode(t, "huge-b")
	a.setBandwidth(40960)
	a.start()
	defer a.stop()

	path := filepath.Join(a.syncDir, "big20.bin")
	writeSparsePattern(t, path, size)
	wantHash := streamHash(t, path)
	t.Logf("source ready: %d bytes, sha256 %s", size, wantHash)

	b.start()
	pairNodes(t, a, b)

	// Wait until B has received at least half (raw bytes, from
	// B's status), then SIGKILL B. Journal + staging must survive.
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		if b.status().BytesReceived >= size/2 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	got := b.status().BytesReceived
	if got < size/2 {
		t.Fatalf("leg 1 stalled: B received only %d bytes in 10m", got)
	}
	t.Logf("leg 1: B received %d bytes (%.1f%%) before kill", got, 100*float64(got)/size)

	b.kill()
	partialDir := filepath.Join(b.syncDir, ".filesync", "partial")
	ents, err := os.ReadDir(partialDir)
	if err != nil || len(ents) == 0 {
		t.Fatalf("no staging file survived the kill (ents=%v err=%v)", ents, err)
	}
	stB, err := store.Open(b.cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	journals, err := stB.ListTransfers()
	stB.Close()
	if err != nil || len(journals) != 1 || journals[0].Path != "big20.bin" {
		t.Fatalf("transfer journal after kill = %+v (err %v), want one for big20.bin", journals, err)
	}

	// Restart B: resume must complete the file exactly, and the
	// new process must report < 60% of 20 GiB received.
	b.start()
	defer b.stop()
	deadline = time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path20At(b)); err == nil && fi.Size() == size {
			break
		}
		time.Sleep(1 * time.Second)
	}
	fi, err := os.Stat(path20At(b))
	if err != nil || fi.Size() != size {
		t.Fatalf("big20.bin never completed after restart (fi=%v err=%v)", fi, err)
	}
	leg2 := b.status().BytesReceived
	if gotHash := streamHash(t, path20At(b)); gotHash != wantHash {
		t.Fatalf("big20.bin content differs from source after resume:\n got %s\nwant %s", gotHash, wantHash)
	}
	t.Logf("leg 2: B received %d bytes (%.1f%% of 20 GiB), hash matches", leg2, 100*float64(leg2)/size)
	if leg2 <= 0 {
		t.Fatal("leg 2 received 0 bytes — resume did not happen, or counters broken")
	}
	if leg2 >= size*60/100 {
		t.Fatalf("leg 2 re-transferred %d bytes (>= 60%% of 20 GiB): resume is not delta-only", leg2)
	}
}

func path20At(n *node) string { return filepath.Join(n.syncDir, "big20.bin") }

// e2eRoot resolves the node-dir root exactly like testBase does
// (baseOverride → FILESYNC_E2E_TMP → ~/.filesync-e2e-tmp), creating
// it if needed, so the disk precondition measures the right
// filesystem before any node exists.
func e2eRoot(t *testing.T) string {
	t.Helper()
	root := baseOverride
	if root == "" {
		root = os.Getenv("FILESYNC_E2E_TMP")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			root = filepath.Join(home, ".filesync-e2e-tmp")
		} else {
			root = os.TempDir()
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// freeBytesAt reports free space on the filesystem holding path.
func freeBytesAt(t *testing.T, path string) uint64 {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		t.Fatalf("statfs %s: %v", path, err)
	}
	return st.Bavail * uint64(st.Bsize)
}

// writeSparsePattern creates a size-byte file at path: sparse
// (Truncate) except for 64 evenly spaced 1 MiB blocks of
// deterministic PRNG content (same recipe as the 1 GiB test), so
// the content is reproducible and the hash has real entropy
// without writing 20 GiB of randomness.
func writeSparsePattern(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	const blocks = 64
	block := make([]byte, 1<<20)
	for i := 0; i < blocks; i++ {
		rng := rand.New(rand.NewSource(int64(3000 + i)))
		_, _ = rng.Read(block)
		if _, err := f.WriteAt(block, int64(i)*(size/blocks)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// streamHash is SHA-256 of a file read in 1 MiB buffers — the
// only safe way to hash a 20 GiB file on this machine.
func streamHash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
