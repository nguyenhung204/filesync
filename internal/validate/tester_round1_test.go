package validate

import (
	"strings"
	"testing"
)

// Tester round 1: total-length checks the builder's tests do not cover —
// length accumulated across segments, length counted in runes (not
// bytes) for multibyte names, and the exact 255-byte segment boundary.
func TestTesterValidateRelPath_TooLongAcrossSegments(t *testing.T) {
	// Each segment is legal on its own (<=255 bytes); the total path is
	// 100+1+100+1+50 = 252 runes > default 240.
	p := strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) + "/" + strings.Repeat("c", 50)
	mustErrCode(t, p, 0, CodeTooLong)
	// Same path is fine with a limit that actually fits it.
	mustValid(t, p, 300, p)
	// Lowering the limit below the total must fail even though the
	// default-limit version would also fail for the same reason.
	mustErrCode(t, p, 251, CodeTooLong)
	mustValid(t, p, 252, p) // exactly at the limit: allowed
}

func TestTesterValidateRelPath_LengthCountedInRunes(t *testing.T) {
	// "é" (U+00E9) is 1 rune but 2 bytes in UTF-8. Two segments of 100
	// é each: 201 runes total, 401 bytes. Spec counts runes, so this
	// must pass the default 240 limit; a byte-counting implementation
	// would wrongly reject it. Each segment is 200 bytes < 255, so the
	// per-segment byte limit does not interfere.
	seg := strings.Repeat("é", 100)
	p := seg + "/" + seg
	if len([]rune(p)) != 201 || len(p) != 401 {
		t.Fatalf("test bug: runes=%d bytes=%d", len([]rune(p)), len(p))
	}
	mustValid(t, p, 0, p)
	// And with a rune limit below 201 it must fail as too long.
	mustErrCode(t, p, 200, CodeTooLong)
	mustValid(t, p, 201, p)
}

func TestTesterValidateRelPath_SegmentBoundary255Bytes(t *testing.T) {
	seg255 := strings.Repeat("s", 255)
	mustValid(t, seg255, 300, seg255) // exactly 255 bytes: allowed
	seg256 := strings.Repeat("s", 256)
	mustErrCode(t, seg256, 300, CodeTooLong) // 256 bytes: too long
	// Multibyte: 128 é = 256 bytes in one segment -> too long by bytes,
	// even though it is only 128 runes.
	mustErrCode(t, strings.Repeat("é", 128), 300, CodeTooLong)
}
