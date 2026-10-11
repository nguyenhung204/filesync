package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesync/internal/config"
	"filesync/internal/logx"
	"filesync/internal/webui"
)

// Integration tests for the web UI against REAL running daemons
// (job 006 acceptance list). Each daemon gets its own borrowed UI
// port so tests never touch the production default.

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// startDaemonCfg is startTestDaemon with a config mutation hook.
func startDaemonCfg(t *testing.T, name string, mutate func(*config.Config)) *testDaemon {
	t.Helper()
	base := t.TempDir()
	cfgDir := filepath.Join(base, "config")
	syncDir := filepath.Join(base, "sync")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ConfigDir = cfgDir
	cfg.SyncDir = syncDir
	cfg.DeviceName = name
	cfg.ListenAddr = "127.0.0.1:0"
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(loaded, logx.New(os.Stderr, logx.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	td := &testDaemon{d: d, cfg: loaded, cancel: cancel, done: make(chan error, 1)}
	go func() { td.done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-td.done:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop in 10s")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if d.Status().ListenAddr != "" {
			return td
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon did not start listening in 10s")
	return nil
}

// uiClient talks to one daemon's UI with its token header.
type uiClient struct {
	base  string
	token string
	hc    *http.Client
}

func waitUI(t *testing.T, td *testDaemon) *uiClient {
	t.Helper()
	port := td.cfg.EffectiveUIPort()
	token, err := webui.ReadToken(td.cfg.ConfigDir)
	if err != nil {
		t.Fatalf("read UI token: %v", err)
	}
	c := &uiClient{base: fmt.Sprintf("http://127.0.0.1:%d", port), token: token, hc: &http.Client{Timeout: 5 * time.Second}}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.get("/api/status"); err == nil {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("web UI did not come up in 15s")
	return nil
}

func (c *uiClient) get(path string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-FileSync-Token", c.token)
	return c.do(req)
}

func (c *uiClient) getList(path string) ([]map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-FileSync-Token", c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *uiClient) post(path string, body map[string]any) (map[string]any, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-FileSync-Token", c.token)
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *uiClient) do(req *http.Request) (map[string]any, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func TestWebUIStatusAndPauseResume(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	port := freePort(t)
	td := startDaemonCfg(t, "ui-one", func(c *config.Config) { c.UIPort = port })
	ui := waitUI(t, td)

	st, err := ui.get("/api/status")
	if err != nil {
		t.Fatal(err)
	}
	if st["state"] != "running" {
		t.Fatalf("state = %v, want running", st["state"])
	}
	wantAddr := fmt.Sprintf("127.0.0.1:%d", port)
	if st["ui_addr"] != wantAddr {
		t.Fatalf("ui_addr = %v, want %s", st["ui_addr"], wantAddr)
	}
	if _, err := ui.post("/api/pause", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "status reports paused", 10*time.Second, func() bool {
		st, err := ui.get("/api/status")
		return err == nil && st["paused"] == true
	})
	if _, err := ui.post("/api/resume", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "status reports resumed", 10*time.Second, func() bool {
		st, err := ui.get("/api/status")
		return err == nil && st["paused"] == false
	})
}

// pairViaHTTP runs the full two-sided HTTP pairing flow and
// cross-checks both fingerprints before confirming.
func pairViaHTTP(t *testing.T, a, b *testDaemon) {
	t.Helper()
	ua, ub := waitUI(t, a), waitUI(t, b)
	codeRes, err := ua.post("/api/pair/code", nil)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := codeRes["code"].(string)
	if len(code) != 6 {
		t.Fatalf("pair code = %v, want 6 digits", codeRes["code"])
	}
	if _, err := ub.post("/api/pair/dial", map[string]any{
		"addr": a.d.Status().ListenAddr, "code": code,
	}); err != nil {
		t.Fatal(err)
	}
	aID, bID := a.d.Status().DeviceID, b.d.Status().DeviceID
	aFP, bFP := a.d.Status().Fingerprint, b.d.Status().Fingerprint

	pendingOf := func(c *uiClient) []map[string]any {
		list, err := c.getList("/api/pending")
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	var aPend, bPend []map[string]any
	waitFor(t, "both sides see a pending session", 15*time.Second, func() bool {
		aPend, bPend = pendingOf(ua), pendingOf(ub)
		return len(aPend) == 1 && len(bPend) == 1
	})
	// Cross-check: each side must see the OTHER's real fingerprint.
	if aPend[0]["device_id"] != bID || aPend[0]["fingerprint"] != bFP {
		t.Fatalf("A pending = %v, want device %s fp %s", aPend[0], bID, bFP)
	}
	if bPend[0]["device_id"] != aID || bPend[0]["fingerprint"] != aFP {
		t.Fatalf("B pending = %v, want device %s fp %s", bPend[0], aID, aFP)
	}
	if _, err := ua.post("/api/pair/confirm", map[string]any{"session_id": aPend[0]["session_id"]}); err != nil {
		t.Fatal(err)
	}
	if _, err := ub.post("/api/pair/confirm", map[string]any{"session_id": bPend[0]["session_id"]}); err != nil {
		t.Fatal(err)
	}
	peersOf := func(c *uiClient, want string) bool {
		list, err := c.getList("/api/peers")
		if err != nil {
			return false
		}
		for _, p := range list {
			if p["device_id"] == want && p["revoked"] == false {
				return true
			}
		}
		return false
	}
	waitFor(t, "peers visible on both sides", 15*time.Second, func() bool {
		return peersOf(ua, bID) && peersOf(ub, aID)
	})
	// And sync actually flows over the HTTP-established trust.
	writeSyncFile(t, a.cfg.SyncDir, "via-http.txt", "paired over http")
	waitFor(t, "file converges", 15*time.Second, func() bool {
		return fileExists(b.cfg.SyncDir, "via-http.txt")
	})
}

func TestWebUIPairTwoDaemons(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	a := startDaemonCfg(t, "ui-alice", func(c *config.Config) { c.UIPort = freePort(t) })
	b := startDaemonCfg(t, "ui-bob", func(c *config.Config) { c.UIPort = freePort(t) })
	pairViaHTTP(t, a, b)
}

func TestWebUIRejectLeavesNoPeers(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	a := startDaemonCfg(t, "ui-carol", func(c *config.Config) { c.UIPort = freePort(t) })
	b := startDaemonCfg(t, "ui-dave", func(c *config.Config) { c.UIPort = freePort(t) })
	ua, ub := waitUI(t, a), waitUI(t, b)
	codeRes, err := ua.post("/api/pair/code", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ub.post("/api/pair/dial", map[string]any{
		"addr": a.d.Status().ListenAddr, "code": codeRes["code"],
	}); err != nil {
		t.Fatal(err)
	}
	var aPend []map[string]any
	waitFor(t, "A sees the pending session", 15*time.Second, func() bool {
		aPend, _ = ua.getList("/api/pending")
		return len(aPend) == 1
	})
	if _, err := ua.post("/api/pair/reject", map[string]any{"session_id": aPend[0]["session_id"]}); err != nil {
		t.Fatal(err)
	}
	// Neither side may hold the other as a peer afterwards.
	time.Sleep(2 * time.Second)
	for name, c := range map[string]*uiClient{"A": ua, "B": ub} {
		peers, err := c.getList("/api/peers")
		if err != nil {
			t.Fatal(err)
		}
		if len(peers) != 0 {
			t.Fatalf("side %s has peers after reject: %v", name, peers)
		}
	}
}

func TestWebUIRevoke(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	a := startDaemonCfg(t, "ui-erin", func(c *config.Config) { c.UIPort = freePort(t) })
	b := startDaemonCfg(t, "ui-frank", func(c *config.Config) { c.UIPort = freePort(t) })
	pairViaHTTP(t, a, b)
	ua := waitUI(t, a)
	bID := b.d.Status().DeviceID
	if _, err := ua.post("/api/peers/revoke", map[string]any{"device_id": bID}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "peer shows revoked", 10*time.Second, func() bool {
		peers, err := ua.getList("/api/peers")
		if err != nil {
			return false
		}
		for _, p := range peers {
			if p["device_id"] == bID {
				return p["revoked"] == true
			}
		}
		return false
	})
	writeSyncFile(t, a.cfg.SyncDir, "after-revoke.txt", "must not sync")
	time.Sleep(3 * time.Second)
	if fileExists(b.cfg.SyncDir, "after-revoke.txt") {
		t.Fatal("file synced to a peer revoked via the UI")
	}
	// Revoking self / unknown devices is a clean 400, not a 500.
	if _, err := ua.post("/api/peers/revoke", map[string]any{"device_id": a.d.Status().DeviceID}); err == nil {
		t.Fatal("revoking self succeeded")
	}
	if _, err := ua.post("/api/peers/revoke", map[string]any{"device_id": "UNKNOWNDEVICE"}); err == nil {
		t.Fatal("revoking unknown device succeeded")
	}
}
