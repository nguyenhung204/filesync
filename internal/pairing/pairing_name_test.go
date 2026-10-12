package pairing

import "testing"

// Job 009 fix: the display name a machine presents while pairing
// is its configured name (Options.Name) on BOTH sides. Before the
// fix the dialer's PairRequest carried the OS hostname (ignoring
// the configured name) and the acceptor's name never reached the
// dialer at all, so the dialer's pending card and stored peer
// record had an empty name — the wizard's fingerprint-confirm
// card could not say WHICH machine was asking.
func TestPairingPresentsConfiguredNames(t *testing.T) {
	a := newPairNode(t, Options{Name: "Máy A xịn"}, nil)
	b := newPairNode(t, Options{Name: "Máy B xịn"}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	pp, err := a.pm.DialPair(t.Context(), a.tm, b.tm.Addr(), code)
	if err != nil {
		t.Fatalf("DialPair: %v", err)
	}
	// Dialer side sees the acceptor's configured name immediately.
	if pp.Name != "Máy B xịn" {
		t.Errorf("dialer's PendingPair.Name = %q, want the acceptor's configured name", pp.Name)
	}
	// Acceptor side sees the dialer's configured name.
	waitPending(t, b.pm, pp.SessionID)
	if st := b.pm.getPending(pp.SessionID); st == nil || st.pair.Name != "Máy A xịn" {
		t.Errorf("acceptor's pending name = %+v, want the dialer's configured name", st)
	}
	if err := a.pm.ConfirmFingerprint(pp.SessionID); err != nil {
		t.Fatalf("ConfirmFingerprint (initiator): %v", err)
	}
	if err := b.pm.ConfirmFingerprint(pp.SessionID); err != nil {
		t.Fatalf("ConfirmFingerprint (acceptor): %v", err)
	}
	// And the names persist into the stored peer records.
	pa, ok := waitPeer(t, a.st, b.id.DeviceID)
	if !ok {
		t.Fatal("initiator store never gained the peer")
	}
	if pa.Name != "Máy B xịn" {
		t.Errorf("initiator's stored peer name = %q, want %q", pa.Name, "Máy B xịn")
	}
	pb, ok := waitPeer(t, b.st, a.id.DeviceID)
	if !ok {
		t.Fatal("acceptor store never gained the peer")
	}
	if pb.Name != "Máy A xịn" {
		t.Errorf("acceptor's stored peer name = %q, want %q", pb.Name, "Máy A xịn")
	}
}

// Empty Options.Name keeps the historical hostname fallback.
func TestPairingNameFallbackHostname(t *testing.T) {
	n := newPairNode(t, Options{}, nil)
	if n.pm.name == "" {
		t.Error("manager name is empty with no Options.Name; want hostname fallback")
	}
	if n.pm.name != deviceName() {
		t.Errorf("manager name = %q, want deviceName() fallback %q", n.pm.name, deviceName())
	}
}
