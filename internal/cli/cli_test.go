package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"filesync/internal/chunk"
	"filesync/internal/config"
	"filesync/internal/store"
)

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newConfigDir seeds a config dir with a config pointing at the
// given listen address.
func newConfigDir(t *testing.T, listen string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ConfigDir = dir
	cfg.SyncDir = filepath.Join(dir, "sync")
	cfg.ListenAddr = listen
	cfg.DeviceName = "testbox"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runCLI(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errs syncBuf
	rc := RunWithIO(args, strings.NewReader(stdin), &out, &errs)
	return out.String(), errs.String(), rc
}

func metasForTest(b []byte) []chunk.ChunkMeta {
	metas, err := chunk.HashChunks(bytes.NewReader(b))
	if err != nil {
		panic(err)
	}
	return metas
}

func TestCLIInitIdempotent(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	out1, _, rc := runCLI(t, "", "--config-dir", dir, "init")
	if rc != 0 {
		t.Fatalf("init rc=%d out=%s", rc, out1)
	}
	m := regexp.MustCompile(`device_id:\s+(\S+)`).FindStringSubmatch(out1)
	if m == nil {
		t.Fatalf("no device id in init output: %q", out1)
	}
	out2, _, rc := runCLI(t, "", "--config-dir", dir, "init")
	if rc != 0 {
		t.Fatalf("second init rc=%d", rc)
	}
	if !strings.Contains(out2, "already initialized") || !strings.Contains(out2, m[1]) {
		t.Fatalf("second init output = %q, want already initialized with same ID", out2)
	}
}

func TestCLIPauseResumeJournal(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "pause"); rc != 0 {
		t.Fatalf("pause rc=%d", rc)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, found, _ := st.JournalGet(pauseJournalKey)
	if !found {
		t.Fatal("pause journal key not set")
	}
	st.Close()
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "resume"); rc != 0 {
		t.Fatalf("resume rc=%d", rc)
	}
	st2, _ := store.Open(dir)
	defer st2.Close()
	_, found, _ = st2.JournalGet(pauseJournalKey)
	if found {
		t.Fatal("pause journal key not cleared by resume")
	}
}

func seedPeer(t *testing.T, dir, id, name string) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutPeer(store.Peer{
		DeviceID: id, Name: name, CertFingerprint: "AA:BB:CC:DD:EE:FF:00:11",
		Addresses: []string{"192.168.1.50:22000"}, PairedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
}

const (
	peerA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	peerB = "AAAAAAAAABBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	peerC = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
)

func TestCLIDevicesListAndJSON(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	seedPeer(t, dir, peerA, "alpha")
	out, _, rc := runCLI(t, "", "--config-dir", dir, "devices")
	if rc != 0 || !strings.Contains(out, "AAAAAAAA…") || !strings.Contains(out, "alpha") {
		t.Fatalf("devices out=%q rc=%d", out, rc)
	}
	out, _, rc = runCLI(t, "", "--config-dir", dir, "--json", "devices")
	if rc != 0 {
		t.Fatalf("devices --json rc=%d", rc)
	}
	var peers []store.Peer
	if err := json.Unmarshal([]byte(out), &peers); err != nil || len(peers) != 1 || peers[0].DeviceID != peerA {
		t.Fatalf("devices --json = %q (err %v)", out, err)
	}
}

func TestCLIRevokeByPrefixAndAmbiguity(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	seedPeer(t, dir, peerA, "alpha")
	seedPeer(t, dir, peerB, "beta")
	// Initialize our own identity so pairing.NewManager works.
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	// Ambiguous prefix (both start with AAAAAAAA).
	_, errs, rc := runCLI(t, "", "--config-dir", dir, "devices", "revoke", "AAAAAAAA")
	if rc == 0 || !strings.Contains(errs, "ambiguous") {
		t.Fatalf("ambiguous revoke rc=%d errs=%q", rc, errs)
	}
	// Unique prefix works.
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "devices", "revoke", "AAAAAAAAAB"); rc != 0 {
		t.Fatalf("unique revoke rc=%d", rc)
	}
	st, _ := store.Open(dir)
	defer st.Close()
	p, found, _ := st.GetPeer(peerB)
	if !found || !p.Revoked {
		t.Fatal("peer B not revoked")
	}
	// Unknown peer -> non-zero exit (store.ErrNotFound path).
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "devices", "revoke", peerC); rc == 0 {
		t.Fatal("revoking an unknown peer succeeded")
	}
	// Short prefix rejected.
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "devices", "revoke", "AAAA"); rc == 0 {
		t.Fatal("revoking with a 4-char prefix succeeded")
	}
}

func TestCLIStatusOffline(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	out, _, rc := runCLI(t, "", "--config-dir", dir, "status")
	if rc != 0 || !strings.Contains(out, "daemon: not running") {
		t.Fatalf("offline status out=%q rc=%d", out, rc)
	}
	out, _, rc = runCLI(t, "", "--config-dir", dir, "--json", "status")
	var obj map[string]any
	if rc != 0 || json.Unmarshal([]byte(out), &obj) != nil || obj["state"] != "offline" {
		t.Fatalf("offline --json = %q rc=%d", out, rc)
	}
}

func TestCLIConflictsListResolve(t *testing.T) {
	dir := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "init"); rc != 0 {
		t.Fatal("init failed")
	}
	cfg, _ := config.Load(dir)
	cp := "f (conflict LOSER000 20240101T000000Z).txt"
	syncDir := cfg.SyncDir
	if err := os.WriteFile(filepath.Join(syncDir, "f.txt"), []byte("winner"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, cp), []byte("loser"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	put := func(p string, content string, updatedBy string) {
		metas := metasForTest([]byte(content))
		e := store.Entry{Path: p, Size: int64(len(content)), Mode: 0o644, Chunks: metas, UpdatedBy: updatedBy}
		e.Vector = e.Vector.Increment("LOSER000")
		if err := st.UpsertEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	put("f.txt", "winner", "WINNER00")
	put(cp, "loser", "LOSER000")
	st.Close()

	out, _, rc := runCLI(t, "", "--config-dir", dir, "conflicts")
	if rc != 0 || !strings.Contains(out, "f.txt") || !strings.Contains(out, "LOSER000") {
		t.Fatalf("conflicts out=%q rc=%d", out, rc)
	}
	out, _, rc = runCLI(t, "", "--config-dir", dir, "--json", "conflicts")
	var infos []map[string]any
	if rc != 0 || json.Unmarshal([]byte(out), &infos) != nil || len(infos) != 1 || infos[0]["original"] != "f.txt" {
		t.Fatalf("conflicts --json = %q rc=%d", out, rc)
	}
	// Resolve keeping theirs: f.txt becomes "loser", copy trashed.
	out, _, rc = runCLI(t, "", "--config-dir", dir, "conflicts", "resolve", "f.txt", "--keep", "theirs")
	if rc != 0 {
		t.Fatalf("resolve rc=%d out=%q", rc, out)
	}
	data, _ := os.ReadFile(filepath.Join(syncDir, "f.txt"))
	if string(data) != "loser" {
		t.Fatalf("f.txt = %q after keep-theirs, want loser", data)
	}
	if _, err := os.Stat(filepath.Join(syncDir, cp)); !os.IsNotExist(err) {
		t.Fatal("conflict copy still at live path after resolve")
	}
	// Missing --keep is an error.
	if _, _, rc := runCLI(t, "", "--config-dir", dir, "conflicts", "resolve", "f.txt"); rc == 0 {
		t.Fatal("resolve without --keep succeeded")
	}
}

func TestCLIPairStandaloneTwoProcesses(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	// Grab a free port for A's listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	dirA := newConfigDir(t, addr)
	dirB := newConfigDir(t, "127.0.0.1:0")
	if _, _, rc := runCLI(t, "", "--config-dir", dirA, "init"); rc != 0 {
		t.Fatal("init A")
	}
	if _, _, rc := runCLI(t, "", "--config-dir", dirB, "init"); rc != 0 {
		t.Fatal("init B")
	}

	var outA, errA syncBuf
	doneA := make(chan int, 1)
	go func() {
		doneA <- RunWithIO([]string{"--config-dir", dirA, "pair"}, strings.NewReader("y\n"), &outA, &errA)
	}()
	// Extract A's code from its stdout.
	codeRe := regexp.MustCompile(`pairing code: (\d{6})`)
	var code string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if m := codeRe.FindStringSubmatch(outA.String()); m != nil {
			code = m[1]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code == "" {
		t.Fatalf("A never printed a code: out=%q err=%q", outA.String(), errA.String())
	}
	var outB, errB syncBuf
	doneB := make(chan int, 1)
	go func() {
		doneB <- RunWithIO([]string{"--config-dir", dirB, "pair", "--to", addr, "--code", code},
			strings.NewReader("y\n"), &outB, &errB)
	}()
	for name, done := range map[string]chan int{"A": doneA, "B": doneB} {
		select {
		case rc := <-done:
			if rc != 0 {
				t.Fatalf("pair %s rc=%d (see logs)", name, rc)
			}
		case <-time.After(45 * time.Second):
			t.Fatalf("pair %s timed out: A=%q/%q B=%q/%q", name, outA.String(), errA.String(), outB.String(), errB.String())
		}
	}
	// Both stores hold the other peer now.
	for _, dir := range []string{dirA, dirB} {
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		peers, _ := st.ListPeers()
		st.Close()
		if len(peers) != 1 || peers[0].Revoked {
			t.Fatalf("store %s peers = %+v, want exactly one trusted peer", dir, peers)
		}
	}
}
