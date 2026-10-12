package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filesync/internal/identity"
	"filesync/internal/logx"
	"filesync/internal/pairing"
	"filesync/internal/store"
	"filesync/internal/transport"
)

// Pair wire types (mirror of the daemon mailbox contract, D6).
type pairRequest struct {
	Action    string `json:"action"`
	Addr      string `json:"addr,omitempty"`
	Code      string `json:"code,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type pairResult struct {
	Action    string `json:"action"`
	OK        bool   `json:"ok"`
	Code      string `json:"code,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"` // Unix seconds
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

const pairTotalTimeout = 7 * time.Minute

// cmdPair pairs with another device. With no flags it creates a
// code (acceptor side); with --to/--code it dials (initiator). It
// drives a running daemon through the pair mailbox, or builds a
// minimal standalone pairing runtime when no daemon is up (Flow 5).
func cmdPair(e *env, args []string) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	to := fs.String("to", "", "address of the device showing a code (host:port)")
	code := fs.String("code", "", "the 6-digit code shown on the other device")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*to == "") != (*code == "") {
		return e.fail("pair: --to and --code must be given together")
	}
	deadline := time.Now().Add(pairTotalTimeout)
	if _, _, live := readLiveStatus(e); live {
		return e.pairViaMailbox(*to, *code, deadline)
	}
	return e.pairStandalone(*to, *code, deadline)
}

// --- shared terminal steps ---

// promptConfirm shows the peer fingerprint and asks for an exact
// "y" (default No — anything else rejects).
func (e *env) promptConfirm(pp pairing.PendingPair) bool {
	name := pp.Name
	if name == "" {
		name = "(unnamed)"
	}
	fmt.Fprintf(e.stdout, "peer: %s (device %s…)\n", name, shortID(pp.DeviceID))
	fmt.Fprintf(e.stdout, "peer fingerprint: %s\n", pp.Fingerprint)
	fmt.Fprint(e.stdout, "confirm pairing? [y/N] ")
	reader := bufio.NewReader(e.stdin)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line) == "y"
}

// waitPeerStored polls the store until the peer record appears
// (pairing finalizes only after BOTH sides confirm).
func (e *env) waitPeerStored(deviceID string, deadline time.Time) bool {
	for time.Now().Before(deadline) {
		_, st, err := e.openStore()
		if err == nil {
			_, found, gerr := st.GetPeer(deviceID)
			st.Close()
			if gerr == nil && found {
				return true
			}
		}
		if !sleepUntil(deadline, 300*time.Millisecond) {
			break
		}
	}
	return false
}

func sleepUntil(deadline time.Time, d time.Duration) bool {
	remain := time.Until(deadline)
	if remain <= 0 {
		return false
	}
	if d > remain {
		d = remain
	}
	time.Sleep(d)
	return true
}

// --- mailbox mode ---

func (e *env) mailboxPath(name string) string {
	return filepath.Join(e.configDir, name)
}

func (e *env) writeMailbox(req pairRequest) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(e.configDir, ".mailbox-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, e.mailboxPath("pair-mailbox.json"))
}

func (e *env) waitMailboxResult(action string, timeout time.Duration) (pairResult, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(e.mailboxPath("pair-mailbox-result.json"))
		if err == nil {
			var res pairResult
			if json.Unmarshal(raw, &res) == nil && res.Action == action {
				return res, true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return pairResult{}, false
}

func (e *env) mailboxAction(req pairRequest, timeout time.Duration) (pairResult, int) {
	_ = os.Remove(e.mailboxPath("pair-mailbox-result.json"))
	if err := e.writeMailbox(req); err != nil {
		return pairResult{}, e.fail("pair: write mailbox: %v", err)
	}
	res, ok := e.waitMailboxResult(req.Action, timeout)
	if !ok {
		return pairResult{}, e.fail("pair: daemon did not answer the %s request in time", req.Action)
	}
	if !res.OK {
		return res, e.fail("pair: %s", res.Error)
	}
	return res, 0
}

// waitPending polls status.json for a pending pair (optionally a
// specific session) until the deadline.
func (e *env) waitPending(sessionID string, deadline time.Time) (pairing.PendingPair, bool) {
	for time.Now().Before(deadline) {
		st, _, live := readLiveStatus(e)
		if live {
			for _, pp := range st.PendingPairs {
				if sessionID == "" || pp.SessionID == sessionID {
					return pp, true
				}
			}
		}
		if !sleepUntil(deadline, 500*time.Millisecond) {
			break
		}
	}
	return pairing.PendingPair{}, false
}

func (e *env) pairViaMailbox(to, code string, deadline time.Time) int {
	var sessionID string
	if to == "" {
		res, rc := e.mailboxAction(pairRequest{Action: "create_code"}, 10*time.Second)
		if rc != 0 {
			return rc
		}
		// The result file contains the code in clear: read it,
		// show it, delete the file immediately (Flow 5 step 1).
		shownCode := res.Code
		expires := time.Unix(res.ExpiresAt, 0).Format("15:04:05")
		_ = os.Remove(e.mailboxPath("pair-mailbox-result.json"))
		fmt.Fprintf(e.stdout, "pairing code: %s\n", shownCode)
		fmt.Fprintf(e.stdout, "expires at %s (5 minutes). On the other device run:\n  filesync pair --to <this-device-address> --code %s\n", expires, shownCode)
		fmt.Fprintln(e.stdout, "waiting for the other device to dial in…")
	} else {
		res, rc := e.mailboxAction(pairRequest{Action: "dial_pair", Addr: to, Code: code}, 30*time.Second)
		if rc != 0 {
			return rc
		}
		sessionID = res.SessionID
		fmt.Fprintln(e.stdout, "proof accepted — fingerprint confirmation is next on BOTH devices.")
	}
	pp, ok := e.waitPending(sessionID, deadline)
	if !ok {
		return e.fail("pair: timed out waiting for the pairing session (7 minutes)")
	}
	confirm := e.promptConfirm(pp)
	action := "reject"
	if confirm {
		action = "confirm"
	}
	if _, rc := e.mailboxAction(pairRequest{Action: action, SessionID: pp.SessionID}, 10*time.Second); rc != 0 {
		return rc
	}
	if !confirm {
		return e.fail("pair: pairing rejected on this side; no trust was stored")
	}
	fmt.Fprintln(e.stdout, "confirmed here — waiting for the other side to confirm too…")
	if !e.waitPeerStored(pp.DeviceID, deadline) {
		return e.fail("pair: timed out waiting for the other side to confirm; no trust was stored")
	}
	fmt.Fprintf(e.stdout, "paired with %s (device %s…)\n", pp.Name, shortID(pp.DeviceID))
	return 0
}

// --- standalone mode (no daemon running) ---

func (e *env) pairStandalone(to, code string, deadline time.Time) int {
	cfg, err := e.loadConfig()
	if err != nil {
		return e.fail("pair: %v", err)
	}
	id, _, err := identity.LoadOrCreate(cfg.ConfigDir)
	if err != nil {
		return e.fail("pair: %v", err)
	}
	st, err := store.Open(cfg.ConfigDir)
	if err != nil {
		return e.fail("pair: %v", err)
	}
	defer st.Close()
	log := logx.New(e.stderr, logx.LevelError)
	topts := transport.DefaultOptions()
	if os.Getenv("FILESYNC_DISABLE_MDNS") == "1" {
		topts.MDNSEnabled = false
	}
	tm := transport.NewManager(id, cfg, st, log, topts)
	pm := pairing.NewManager(id, st, log, tm, pairing.Options{Name: cfg.DeviceName})
	tm.SetPairingGate(pm)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		// No sync engine in standalone mode: trusted conns are
		// closed immediately (the daemon owns syncing).
		serveErr <- tm.Serve(ctx, func(_ context.Context, c *transport.Conn) { _ = c.Close() }, pm.HandleConn)
	}()
	proposed := make(chan pairing.PendingPair, 1)
	pm.OnPairProposed(func(pp pairing.PendingPair) {
		select {
		case proposed <- pp:
		default:
		}
	})

	var pp pairing.PendingPair
	if to == "" {
		codeStr, expires, err := pm.CreateCode()
		if err != nil {
			return e.fail("pair: create code: %v", err)
		}
		fmt.Fprintf(e.stdout, "pairing code: %s\n", codeStr)
		fmt.Fprintf(e.stdout, "expires at %s (5 minutes). On the other device run:\n  filesync pair --to %s --code %s\n",
			expires.Format("15:04:05"), cfg.ListenAddr, codeStr)
		fmt.Fprintln(e.stdout, "waiting for the other device to dial in…")
		select {
		case pp = <-proposed:
		case err := <-serveErr:
			return e.fail("pair: listener failed: %v", err)
		case <-ctx.Done():
			return e.fail("pair: timed out (7 minutes)")
		}
	} else {
		var err error
		pp, err = pm.DialPair(ctx, tm, to, code)
		if err != nil {
			return e.fail("pair: %v", err)
		}
		fmt.Fprintln(e.stdout, "proof accepted — fingerprint confirmation is next on BOTH devices.")
	}
	if !e.promptConfirm(pp) {
		_ = pm.RejectFingerprint(pp.SessionID)
		return e.fail("pair: pairing rejected on this side; no trust was stored")
	}
	if err := pm.ConfirmFingerprint(pp.SessionID); err != nil {
		return e.fail("pair: confirm: %v", err)
	}
	fmt.Fprintln(e.stdout, "confirmed here — waiting for the other side to confirm too…")
	for time.Now().Before(deadline) {
		if _, found, err := st.GetPeer(pp.DeviceID); err == nil && found {
			fmt.Fprintf(e.stdout, "paired with %s (device %s…)\n", pp.Name, shortID(pp.DeviceID))
			return 0
		}
		select {
		case <-ctx.Done():
			return e.fail("pair: timed out waiting for the other side to confirm; no trust was stored")
		case <-time.After(300 * time.Millisecond):
		}
	}
	return e.fail("pair: timed out waiting for the other side to confirm; no trust was stored")
}
