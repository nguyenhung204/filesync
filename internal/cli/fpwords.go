package cli

import (
	"encoding/hex"
	"strings"
)

// fingerprintWords turns a fingerprint (uppercase hex grouped in
// blocks of 4, as rendered by identity.FingerprintFromCertDER)
// into the 8-word human comparison phrase shown next to it
// ("Từ đối chiếu: ..."). The first 8 bytes of the fingerprint each
// pick one word from fpWordsTable — the same table and algorithm
// the Web UI uses (internal/webui/static/words.js), pinned by the
// shared test vector in fpwords_test.go. Presentation only: the
// pairing proof and fingerprint itself are unchanged. Returns nil
// for malformed input.
func fingerprintWords(fp string) []string {
	raw, err := hex.DecodeString(strings.ReplaceAll(fp, " ", ""))
	if err != nil || len(raw) < 8 {
		return nil
	}
	words := make([]string, 8)
	for i := 0; i < 8; i++ {
		words[i] = fpWordsTable[raw[i]]
	}
	return words
}
