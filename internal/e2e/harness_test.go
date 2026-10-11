//go:build e2e

// Package e2e_test runs the real filesync binary as subprocesses:
// three-node convergence, offline conflicts, crash-resume of a
// 1 GiB file, and the 10k-file workload. Run with:
//
//	go test -tags=e2e ./internal/e2e/... -timeout 20m
package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"filesync/internal/config"
	"filesync/internal/daemon"
)

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

func binary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		root, err := repoRoot()
		if err != nil {
			binErr = err
			return
		}
		// Fixed path on the big disk: a fresh os.MkdirTemp per
		// run leaked an 18MB binary dir into the small /tmp
		// tmpfs every suite run.
		home, herr := os.UserHomeDir()
		if herr != nil {
			binErr = herr
			return
		}
		dir := filepath.Join(home, ".filesync-e2e-tmp", "bin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "filesync")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/filesync")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("build binary: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd, nil
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			return "", fmt.Errorf("go.mod not found above %s", wd)
		}
		wd = parent
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// node is one filesync process pair (init'ed config + serve).
type node struct {
	t          *testing.T
	name       string
	base       string
	cfgDir     string
	syncDir    string
	listenAddr string
	deviceID   string
	bandwidth  int // KiB/s, 0 = unlimited

	cmd    *exec.Cmd
	stderr *lockedBuffer
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// baseOverride, when set by a test, replaces the default node-dir
// root (tests run sequentially, so a package var is safe).
var baseOverride string

// tmpFreeBytes reports free space on os.TempDir's filesystem.
func tmpFreeBytes(t *testing.T) uint64 {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(os.TempDir(), &st); err != nil {
		return 0
	}
	return st.Bavail * uint64(st.Bsize)
}

// testBase allocates a node dir on the big disk: /tmp is a small
// tmpfs on this machine (512 MiB) and the resume test moves 1 GiB
// files around, so t.TempDir() is not an option there.
func testBase(t *testing.T) string {
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
	base, err := os.MkdirTemp(root, "node-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return base
}

// newNode prepares a node's dirs and config and runs `init`.
func newNode(t *testing.T, name string) *node {
	t.Helper()
	base := testBase(t)
	n := &node{
		t:       t,
		name:    name,
		base:    base,
		cfgDir:  filepath.Join(base, "config"),
		syncDir: filepath.Join(base, "sync"),
	}
	n.listenAddr = freeAddr(t)
	if err := os.MkdirAll(n.cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(n.syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ConfigDir = n.cfgDir
	cfg.SyncDir = n.syncDir
	cfg.DeviceName = name
	cfg.ListenAddr = n.listenAddr
	cfg.BandwidthLimitKBps = n.bandwidth
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(binary(t), "--config-dir", n.cfgDir, "init").CombinedOutput()
	if err != nil {
		t.Fatalf("init %s: %v\n%s", name, err, out)
	}
	m := regexp.MustCompile(`device_id:\s+(\S+)`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("init %s: no device id in %q", name, out)
	}
	n.deviceID = m[1]
	return n
}

// setBandwidth rewrites the node's config (before start).
func (n *node) setBandwidth(kbps int) {
	cfg, err := config.Load(n.cfgDir)
	if err != nil {
		n.t.Fatal(err)
	}
	cfg.BandwidthLimitKBps = kbps
	if err := cfg.Save(); err != nil {
		n.t.Fatal(err)
	}
}

func (n *node) start() {
	n.t.Helper()
	if n.cmd != nil {
		n.t.Fatalf("node %s already running", n.name)
	}
	n.stderr = &lockedBuffer{}
	cmd := exec.Command(binary(n.t), "--config-dir", n.cfgDir, "serve")
	cmd.Env = append(os.Environ(), "FILESYNC_DISABLE_MDNS=1")
	cmd.Stdout = n.stderr
	cmd.Stderr = n.stderr
	if err := cmd.Start(); err != nil {
		n.t.Fatalf("start %s: %v", n.name, err)
	}
	n.cmd = cmd
	st := n.waitStatus(func(s daemon.Status) bool {
		return (s.State == "running" || s.State == "paused") && s.ListenAddr != ""
	}, 60*time.Second)
	if st.DeviceID != n.deviceID {
		n.t.Fatalf("node %s: status device %q != init device %q", n.name, st.DeviceID, n.deviceID)
	}
	if st.ListenAddr != n.listenAddr {
		n.t.Fatalf("node %s: status listen %q != configured %q", n.name, st.ListenAddr, n.listenAddr)
	}
}

// stop SIGTERMs the daemon and waits for a clean exit + final
// "stopped" status.
func (n *node) stop() {
	n.t.Helper()
	if n.cmd == nil {
		return
	}
	_ = n.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- n.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			n.t.Fatalf("node %s exit: %v\nlogs:\n%s", n.name, err, n.stderr.String())
		}
	case <-time.After(20 * time.Second):
		_ = n.cmd.Process.Kill()
		n.t.Fatalf("node %s did not stop in 20s\nlogs:\n%s", n.name, n.stderr.String())
	}
	n.cmd = nil
}

// kill SIGKILLs the daemon (crash simulation).
func (n *node) kill() {
	n.t.Helper()
	if n.cmd == nil {
		return
	}
	_ = n.cmd.Process.Kill()
	_ = n.cmd.Wait()
	n.cmd = nil
}

// status reads the node's current status.json (zero value when
// absent or unparsable).
func (n *node) status() daemon.Status {
	raw, err := os.ReadFile(filepath.Join(n.cfgDir, "status.json"))
	if err != nil {
		return daemon.Status{}
	}
	var st daemon.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return daemon.Status{}
	}
	return st
}

func (n *node) waitStatus(cond func(daemon.Status) bool, timeout time.Duration) daemon.Status {
	n.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := n.status()
		if cond(st) {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
	n.t.Fatalf("node %s: status condition not met in %v (last=%+v)\nlogs:\n%s",
		n.name, timeout, n.status(), n.stderr.String())
	return daemon.Status{}
}

func (n *node) writeFile(rel string, data []byte) {
	n.t.Helper()
	p := filepath.Join(n.syncDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		n.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		n.t.Fatal(err)
	}
}

func (n *node) readFile(rel string) ([]byte, bool) {
	data, err := os.ReadFile(filepath.Join(n.syncDir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, false
	}
	return data, true
}

// treeHashes maps every synced file (excluding .filesync) to its
// SHA-256 hex.
func (n *node) treeHashes() map[string]string {
	out := map[string]string{}
	_ = filepath.Walk(n.syncDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".filesync" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		rel, _ := filepath.Rel(n.syncDir, p)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return out
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// --- pairing through the real mailbox files; the test plays the
// user on both sides ---

type mailboxReq struct {
	Action    string `json:"action"`
	Addr      string `json:"addr,omitempty"`
	Code      string `json:"code,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type mailboxRes struct {
	Action    string `json:"action"`
	OK        bool   `json:"ok"`
	Code      string `json:"code,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (n *node) writeMailbox(req mailboxReq) {
	raw, _ := json.Marshal(req)
	tmp := filepath.Join(n.cfgDir, ".mailbox-test.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		n.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(n.cfgDir, "pair-mailbox.json")); err != nil {
		n.t.Fatal(err)
	}
}

func (n *node) waitMailboxResult(action string, timeout time.Duration) mailboxRes {
	n.t.Helper()
	deadline := time.Now().Add(timeout)
	path := filepath.Join(n.cfgDir, "pair-mailbox-result.json")
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var res mailboxRes
			if json.Unmarshal(raw, &res) == nil && res.Action == action {
				return res
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	n.t.Fatalf("node %s: no mailbox result for %s in %v\nlogs:\n%s", n.name, action, timeout, n.stderr.String())
	return mailboxRes{}
}

func (n *node) waitPending(timeout time.Duration) (sessionID string) {
	n.t.Helper()
	st := n.waitStatus(func(s daemon.Status) bool { return len(s.PendingPairs) > 0 }, timeout)
	return st.PendingPairs[0].SessionID
}

// pairNodes pairs acceptor <- initiator through the real mailbox
// protocol: the initiator dials the acceptor's address with the
// acceptor's code; the test confirms on both sides.
func pairNodes(t *testing.T, acceptor, initiator *node) {
	t.Helper()
	_ = os.Remove(filepath.Join(acceptor.cfgDir, "pair-mailbox-result.json"))
	acceptor.writeMailbox(mailboxReq{Action: "create_code"})
	res := acceptor.waitMailboxResult("create_code", 15*time.Second)
	if !res.OK || res.Code == "" {
		t.Fatalf("create_code on %s: %+v", acceptor.name, res)
	}
	code := res.Code
	// The user-side contract: the code file is deleted after reading.
	_ = os.Remove(filepath.Join(acceptor.cfgDir, "pair-mailbox-result.json"))

	initiator.writeMailbox(mailboxReq{Action: "dial_pair", Addr: acceptor.listenAddr, Code: code})
	dres := initiator.waitMailboxResult("dial_pair", 30*time.Second)
	if !dres.OK {
		t.Fatalf("dial_pair on %s: %+v", initiator.name, dres)
	}
	sessA := acceptor.waitPending(15 * time.Second)
	sessB := initiator.waitPending(15 * time.Second)
	acceptor.writeMailbox(mailboxReq{Action: "confirm", SessionID: sessA})
	initiator.writeMailbox(mailboxReq{Action: "confirm", SessionID: sessB})
	if r := acceptor.waitMailboxResult("confirm", 15*time.Second); !r.OK {
		t.Fatalf("confirm on %s: %+v", acceptor.name, r)
	}
	if r := initiator.waitMailboxResult("confirm", 15*time.Second); !r.OK {
		t.Fatalf("confirm on %s: %+v", initiator.name, r)
	}
	for _, n := range []*node{acceptor, initiator} {
		n.waitStatus(func(s daemon.Status) bool {
			for _, p := range s.Peers {
				if !p.Revoked {
					return true
				}
			}
			return false
		}, 20*time.Second)
	}
}

// waitTreeEqual polls until all nodes' trees hash-identical.
func waitTreeEqual(t *testing.T, timeout time.Duration, nodes ...*node) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		want := nodes[0].treeHashes()
		ok := true
		for _, n := range nodes[1:] {
			got := n.treeHashes()
			if len(got) != len(want) {
				ok = false
				break
			}
			for k, v := range want {
				if got[k] != v {
					ok = false
					break
				}
			}
			if !ok {
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, n := range nodes {
		t.Logf("node %s tree: %v", n.name, n.treeHashes())
	}
	t.Fatalf("trees did not converge in %v", timeout)
}
