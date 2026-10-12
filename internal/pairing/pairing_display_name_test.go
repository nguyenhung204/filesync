package pairing

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Job 009 round-2 reviewer NITs: a configured device name over
// the proto Header.Name limit (128 bytes) made every pairing
// frame fail validation and broke pairing outright; and a peer
// name carrying control characters could smuggle ANSI escapes
// into the terminal that prints the confirm prompt.

func TestTruncateDisplayName(t *testing.T) {
	if got := truncateDisplayName(strings.Repeat("a", 200)); len(got) != 128 {
		t.Errorf("200 ASCII bytes: got %d bytes, want 128", len(got))
	}
	// "á" is 2 bytes: 100 runes = 200 bytes cuts to exactly 64.
	if got := truncateDisplayName(strings.Repeat("á", 100)); len(got) != 128 || !utf8.ValidString(got) {
		t.Errorf("multibyte: got %d bytes (valid=%v), want 128 valid bytes", len(got), utf8.ValidString(got))
	}
	// A rune straddling the boundary is dropped whole, never split.
	name := strings.Repeat("a", 127) + "á" // 129 bytes
	if got := truncateDisplayName(name); got != strings.Repeat("a", 127) {
		t.Errorf("boundary rune: got %q (len %d), want the 127 ASCII bytes", got, len(got))
	}
	if got := truncateDisplayName("Máy ngắn"); got != "Máy ngắn" {
		t.Errorf("short name changed: %q", got)
	}
}

func TestPairingLongNameStillPairs(t *testing.T) {
	long := strings.Repeat("á", 100) // 200 bytes before truncation
	a := newPairNode(t, Options{Name: long}, nil)
	b := newPairNode(t, Options{Name: "Máy B"}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	pp, err := a.pm.DialPair(t.Context(), a.tm, b.tm.Addr(), code)
	if err != nil {
		t.Fatalf("DialPair with a >128-byte local name: %v", err)
	}
	waitPending(t, b.pm, pp.SessionID)
	st := b.pm.getPending(pp.SessionID)
	if st == nil {
		t.Fatal("acceptor has no pending session")
	}
	if len(st.pair.Name) > 128 || !utf8.ValidString(st.pair.Name) {
		t.Errorf("acceptor sees %d bytes (valid=%v), want <=128 valid bytes", len(st.pair.Name), utf8.ValidString(st.pair.Name))
	}
}

func TestCleanPeerName(t *testing.T) {
	in := "Máy \x1b[31mđộc\r\n\x1b[0m \u202eđảo"
	want := "Máy [31mđộc[0m đảo"
	if got := cleanPeerName(in); got != want {
		t.Errorf("cleanPeerName(%q) = %q, want %q", in, got, want)
	}
	if got := cleanPeerName("Máy bình thường"); got != "Máy bình thường" {
		t.Errorf("clean name changed: %q", got)
	}
}

// End to end: the name B records for A — pending card and stored
// peer alike — is the sanitized one, while pairing still works.
func TestPairingSanitizesPeerName(t *testing.T) {
	a := newPairNode(t, Options{Name: "Ác\x1b[2J\nquỷ"}, nil)
	b := newPairNode(t, Options{Name: "Máy B"}, nil)
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
	waitPending(t, b.pm, pp.SessionID)
	st := b.pm.getPending(pp.SessionID)
	if st == nil {
		t.Fatal("acceptor has no pending session")
	}
	if st.pair.Name != "Ác[2Jquỷ" {
		t.Errorf("acceptor pending name = %q, want sanitized %q", st.pair.Name, "Ác[2Jquỷ")
	}
	if err := a.pm.ConfirmFingerprint(pp.SessionID); err != nil {
		t.Fatalf("ConfirmFingerprint (initiator): %v", err)
	}
	if err := b.pm.ConfirmFingerprint(pp.SessionID); err != nil {
		t.Fatalf("ConfirmFingerprint (acceptor): %v", err)
	}
	pb, ok := waitPeer(t, b.st, a.id.DeviceID)
	if !ok {
		t.Fatal("acceptor store never gained the peer")
	}
	if pb.Name != "Ác[2Jquỷ" {
		t.Errorf("stored peer name = %q, want sanitized %q", pb.Name, "Ác[2Jquỷ")
	}
}
