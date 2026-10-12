package cli

import (
	"strings"
	"testing"
	"unicode"
)

// TestFingerprintWordsVector pins the shared Go/JS test vector:
// this fingerprint is sha256("filesync") in the canonical grouped
// rendering, and the expected words were produced by the generator
// (round-3/make_fpwords.py). The Playwright harness asserts the
// same vector against internal/webui/static/words.js, so the two
// tables can never drift apart silently.
func TestFingerprintWordsVector(t *testing.T) {
	fp := "60A9 6B16 1BB4 7321 7EA3 88D3 42DB B111 6F63 EEDE 7EED BAE8 5B83 2500 1BF5 8BB2"
	want := []string{"gừng", "thôn", "lạc", "chim", "ong", "quên", "canh", "cáo"}
	got := fingerprintWords(fp)
	if len(got) != len(want) {
		t.Fatalf("fingerprintWords = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("word %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestFingerprintWordsMalformed(t *testing.T) {
	for _, fp := range []string{"", "xyz", "60A9", "60A9 6B16"} {
		if got := fingerprintWords(fp); got != nil {
			t.Errorf("fingerprintWords(%q) = %v, want nil", fp, got)
		}
	}
}

// TestFPWordsTableInvariants guards the properties the generator
// filtered for, so a hand edit cannot quietly reintroduce a
// confusable or unusable entry.
func TestFPWordsTableInvariants(t *testing.T) {
	seen := make(map[string]bool, 256)
	for i, w := range fpWordsTable {
		if w == "" {
			t.Fatalf("entry %d is empty", i)
		}
		if seen[w] {
			t.Fatalf("entry %d %q duplicates an earlier word", i, w)
		}
		seen[w] = true
		if strings.TrimSpace(w) != w || strings.ContainsAny(w, " \t\n") {
			t.Fatalf("entry %d %q contains whitespace", i, w)
		}
		for _, r := range w {
			if unicode.IsControl(r) {
				t.Fatalf("entry %d %q contains a control character", i, w)
			}
		}
	}
}
