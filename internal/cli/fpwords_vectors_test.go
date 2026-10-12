package cli

import "testing"

// TestFingerprintWordsCrossVectors pins additional Go/JS agreement
// vectors (added by reviewer-test, job 009 round 3). The expected
// phrases were produced by the JavaScript implementation
// (internal/webui/static/words.js fpWords) for fingerprints derived
// from sha256(seed); the Go table/algorithm must reproduce them
// exactly, so the two implementations cannot drift on inputs beyond
// the single shared vector in fpwords_test.go.
func TestFingerprintWordsCrossVectors(t *testing.T) {
	cases := []struct {
		fp   string
		want []string
	}{
		{"8ED3 F6AD 685B 959E AD70 2251 8E1A F76C D816 F8E8 EC7C CDDA 1ED4 018E 8F22 23F8",
			[]string{"dốc", "chum", "hẻm", "leo", "khoai", "lan", "lửa", "xuân"}},
		{"F144 A690 7DC4 284D 1F9F E6A7 D9B9 FF53 C02C 1D07 BA68 F24D 413D 7FF7 F757 A782",
			[]string{"môi", "mận", "đập", "vịnh", "mây", "dép", "ngỗng", "nhãn"}},
		{"B9DD 960C 1753 459A 7811 5D3C B845 A57D 924B 6877 E805 B08B D010 86CC DF34 433C",
			[]string{"nến", "chỉ", "khói", "gà", "ếch", "me", "xoài", "lá"}},
		{"4F4A 9410 FFCD F895 C4AD B880 659E 9B5C 0DD1 F23A 3079 0684 340B 3EAA CB04 5398",
			[]string{"hồng", "chuối", "sao", "ngựa", "rộng", "thìa", "bếp", "lửa"}},
		{"092C 79E8 F80E 559E 404B CF66 0C48 F352 2B67 ABA9 FF14 84B0 367E 1A4D DEF7 431D",
			[]string{"mười", "chuột", "rượu", "tay", "bếp", "bò", "sung", "xuân"}},
	}
	for _, c := range cases {
		got := fingerprintWords(c.fp)
		if len(got) != len(c.want) {
			t.Fatalf("fingerprintWords(%q) = %v, want %v", c.fp, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("fingerprintWords(%q) word %d = %q, want %q", c.fp, i, got[i], c.want[i])
			}
		}
	}
}
