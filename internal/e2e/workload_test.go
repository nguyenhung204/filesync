//go:build e2e

package e2e_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"filesync/internal/store"
)

// TestE2EResume1GiB: a 1 GiB incompressible file is throttled so
// the transfer takes minutes; B's daemon is SIGKILLed past the
// halfway point; after restart B must receive ONLY the missing
// remainder — measured from B's fresh-process byte counters:
// leg-2 bytes_received must be > 0 and < 60% of 1 GiB.
func TestE2EResume1GiB(t *testing.T) {
	const size = 1 << 30
	a := newNode(t, "big-a")
	b := newNode(t, "big-b")
	// ~5 MiB/s: leg 1 needs ~2 minutes to cross 512 MiB.
	a.setBandwidth(40960)
	a.start()
	defer a.stop()
	b.start()

	// Deterministic incompressible payload: a seeded PRNG stream
	// per 1 MiB block (reproducible without storing 1 GiB in RAM).
	path := filepath.Join(a.syncDir, "big.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 1<<20)
	for i := 0; i < size/(1<<20); i++ {
		rng := rand.New(rand.NewSource(int64(1000 + i)))
		_, _ = rng.Read(block)
		if _, err := f.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	wantHash := fileHash(t, path)

	pairNodes(t, a, b)

	// Wait until B has received at least half, from B's status.
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		if b.status().BytesReceived >= size/2 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	got := b.status().BytesReceived
	if got < size/2 {
		t.Fatalf("leg 1 stalled: B received only %d bytes in 6m", got)
	}
	t.Logf("leg 1: B received %d bytes before kill", got)

	// SIGKILL B. Journal + staging must survive on disk.
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
	if err != nil || len(journals) != 1 || journals[0].Path != "big.bin" {
		t.Fatalf("transfer journal after kill = %+v (err %v), want one for big.bin", journals, err)
	}

	// Restart B: the resume must complete the file exactly, and
	// the new process must report < 60% of 1 GiB received.
	b.start()
	defer b.stop()
	deadline = time.Now().Add(6 * time.Minute)
	var leg2 int64
	for time.Now().Before(deadline) {
		data, ok := b.readFile("big.bin")
		if ok && int64(len(data)) == size {
			break
		}
		time.Sleep(1 * time.Second)
		leg2 = b.status().BytesReceived
	}
	leg2 = b.status().BytesReceived
	data, ok := b.readFile("big.bin")
	if !ok {
		t.Fatal("big.bin never arrived after restart")
	}
	if hashOf(data) != wantHash {
		t.Fatal("big.bin content differs from source after resume")
	}
	t.Logf("leg 2: B received %d bytes (%.1f%% of 1 GiB)", leg2, 100*float64(leg2)/size)
	if leg2 <= 0 {
		t.Fatal("leg 2 received 0 bytes — resume did not happen, or counters broken")
	}
	if leg2 >= size*60/100 {
		t.Fatalf("leg 2 re-transferred %d bytes (>= 60%% of 1 GiB): resume is not delta-only", leg2)
	}
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return hashOf(data)
}

// TestWorkload10kFiles: 10 000 small files created before A's
// daemon starts. Criteria: A's initial scan ≤ 60s, both daemons'
// idle RSS ≤ 150 MiB after convergence.
func TestWorkload10kFiles(t *testing.T) {
	const n = 10000
	a := newNode(t, "many-a")
	b := newNode(t, "many-b")
	// Content derived from the file name: deterministic 200B–4KiB.
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("dir%02d/file-%05d.txt", i%20, i)
		seed := []byte(name)
		size := 200 + (i*37)%3800
		content := bytes.Repeat(seed, size/len(seed)+1)[:size]
		a.writeFile(name, content)
	}
	a.start()
	defer a.stop()
	scanMs := a.status().InitialScanMs
	t.Logf("initial_scan_ms for 10k files: %d", scanMs)
	if scanMs > 60000 {
		t.Fatalf("initial scan took %d ms (> 60000)", scanMs)
	}
	b.start()
	defer b.stop()
	pairNodes(t, a, b)
	waitTreeEqual(t, 120*time.Second, a, b)

	// Idle for 5s, then measure RSS of both daemon processes.
	time.Sleep(5 * time.Second)
	for _, nd := range []*node{a, b} {
		rss, ok := processRSSKiB(nd.status().PID)
		if !ok {
			t.Logf("node %s: /proc unavailable, RSS check skipped", nd.name)
			continue
		}
		t.Logf("node %s idle RSS: %d KiB", nd.name, rss)
		if rss > 150*1024 {
			t.Fatalf("node %s idle RSS %d KiB exceeds 150 MiB", nd.name, rss)
		}
	}
}

func processRSSKiB(pid int) (int64, bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil {
					return v, true
				}
			}
		}
	}
	return 0, false
}

// TestWorkloadLatencyP95: 50 sequential small edits at A; measure
// write→visible-at-B latency; p95 must be ≤ 10s. The measured p95
// is logged for build.md either way.
func TestWorkloadLatencyP95(t *testing.T) {
	a := newNode(t, "lat-a")
	b := newNode(t, "lat-b")
	a.start()
	defer a.stop()
	b.start()
	defer b.stop()
	pairNodes(t, a, b)

	const rounds = 50
	lat := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		name := fmt.Sprintf("lat-%02d.txt", i)
		content := []byte(fmt.Sprintf("latency probe %d", i))
		start := time.Now()
		a.writeFile(name, content)
		want := hashOf(content)
		for {
			tree := b.treeHashes()
			if tree[name] == want {
				break
			}
			if time.Since(start) > 30*time.Second {
				t.Fatalf("edit %d not visible at B after 30s", i)
			}
			time.Sleep(25 * time.Millisecond)
		}
		lat = append(lat, time.Since(start))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p95 := lat[int(0.95*float64(len(lat)))-1]
	t.Logf("latency p95 over %d edits: %v (max %v)", rounds, p95, lat[len(lat)-1])
	if p95 > 10*time.Second {
		t.Fatalf("p95 latency %v exceeds 10s", p95)
	}
}
