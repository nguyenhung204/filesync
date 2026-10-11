package validate

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// Job 002c, mục 1 (GATE): invalid UTF-8 must be rejected with
// CodeForbiddenChar before NFC normalization can mangle the bytes.
func TestValidateRelPath_RejectsInvalidUTF8(t *testing.T) {
	for _, p := range []string{"a/\xff\xfe.txt", "\xff", "dir/\xc3\x28.txt"} {
		_, err := ValidateRelPath(p, 0)
		if err == nil {
			t.Errorf("ValidateRelPath(%q): expected error, got nil", p)
			continue
		}
		if !HasCode(err, CodeForbiddenChar) {
			t.Errorf("ValidateRelPath(%q): err = %v, want code %s", p, err, CodeForbiddenChar)
		}
	}
}

func TestValidateSegment_RejectsInvalidUTF8(t *testing.T) {
	err := ValidateSegment("\xff\xfe")
	if !HasCode(err, CodeForbiddenChar) {
		t.Fatalf("ValidateSegment(invalid UTF-8): err = %v, want code %s", err, CodeForbiddenChar)
	}
}

// Job 002c, mục 2 (GATE): CONIN$ / CONOUT$ are reserved device names on
// every platform, with or without an extension.
func TestValidateRelPath_ReservedNamesDollarDevices(t *testing.T) {
	for _, p := range []string{"CONIN$", "conin$.txt", "CONOUT$", "ConOut$.md"} {
		if _, err := ValidateRelPath(p, 0); !HasCode(err, CodeReservedName) {
			t.Errorf("ValidateRelPath(%q): err = %v, want code %s", p, err, CodeReservedName)
		}
	}
	for _, p := range []string{"CONIN", "CONOUTX$", "$CONIN"} {
		if _, err := ValidateRelPath(p, 0); err != nil {
			t.Errorf("ValidateRelPath(%q): err = %v, want valid", p, err)
		}
	}
}

// Job 002c, mục 4 (NIT): ValidateSegment applies the 255-byte limit to the
// NFC form, exactly like ValidateRelPath. 100 x "e + combining acute" is
// 300 bytes raw (NFD) but 200 bytes after NFC.
func TestValidateSegment_NFDLengthAfterNFC(t *testing.T) {
	seg := strings.Repeat("e\u0301", 100)
	if len(seg) != 300 {
		t.Fatalf("test setup: raw segment is %d bytes, want 300", len(seg))
	}
	if err := ValidateSegment(seg); err != nil {
		t.Errorf("ValidateSegment(NFD segment): err = %v, want nil", err)
	}
	if _, err := ValidateRelPath(seg, 0); err != nil {
		t.Errorf("ValidateRelPath(NFD segment): err = %v, want nil", err)
	}
}

// Job 002c, mục 4 (NIT): Error.Path must carry the caller's original
// (pre-NFC) string, byte for byte.
func TestValidate_ErrorPathKeepsOriginal(t *testing.T) {
	orig := "cafe\u0301/../x.txt" // NFD "café" segment, then a traversal segment
	if norm.NFC.String(orig) == orig {
		t.Fatal("test setup: input must be in NFD form")
	}
	_, err := ValidateRelPath(orig, 0)
	if !HasCode(err, CodeTraversal) {
		t.Fatalf("ValidateRelPath(NFD + ..): err = %v, want code %s", err, CodeTraversal)
	}
	var ve *Error
	if !errors.As(err, &ve) {
		t.Fatalf("error is %T, want *Error", err)
	}
	if ve.Path != orig {
		t.Errorf("Error.Path = %q, want original input %q", ve.Path, orig)
	}
}
