package validate

import (
	"strings"
	"testing"
)

func mustErrCode(t *testing.T, path string, maxLen int, want Code) {
	t.Helper()
	got, err := ValidateRelPath(path, maxLen)
	if err == nil {
		t.Fatalf("ValidateRelPath(%q) = %q, want error %s", path, got, want)
	}
	if !HasCode(err, want) {
		t.Fatalf("ValidateRelPath(%q) error = %v, want code %s", path, err, want)
	}
}

func mustValid(t *testing.T, path string, maxLen int, want string) {
	t.Helper()
	got, err := ValidateRelPath(path, maxLen)
	if err != nil {
		t.Fatalf("ValidateRelPath(%q) error = %v, want %q", path, err, want)
	}
	if got != want {
		t.Fatalf("ValidateRelPath(%q) = %q, want %q", path, got, want)
	}
}

func TestValidateRelPath_Valid(t *testing.T) {
	mustValid(t, "docs/a-b_c.txt", 0, "docs/a-b_c.txt")
	mustValid(t, "a/b/c", 0, "a/b/c")
	p240 := strings.Repeat("a", 120) + "/" + strings.Repeat("b", 119)
	if len([]rune(p240)) != 240 {
		t.Fatal("test bug: path is not 240 runes")
	}
	mustValid(t, p240, 0, p240)
}

func TestValidateRelPath_Traversal(t *testing.T) {
	for _, p := range []string{"../secret", "a/../../b", "a/.."} {
		mustErrCode(t, p, 0, CodeTraversal)
	}
}

func TestValidateRelPath_Absolute(t *testing.T) {
	for _, p := range []string{"/etc/passwd", `C:\Windows\x`, "C:/x", "//server/share", `\\server\share`} {
		mustErrCode(t, p, 0, CodeAbsolute)
	}
}

func TestValidateRelPath_WindowsForbiddenChars(t *testing.T) {
	for _, ch := range []string{":", "*", "?", `"`, "<", ">", "|"} {
		mustErrCode(t, "dir/file"+ch+"x.txt", 0, CodeForbiddenChar)
	}
	mustErrCode(t, `a\b`, 0, CodeForbiddenChar)
	mustErrCode(t, `dir/sub\file.txt`, 0, CodeForbiddenChar)
}

func TestValidateRelPath_ReservedNames(t *testing.T) {
	for _, p := range []string{"CON", "con.txt", "PRN", "AUX.md", "NUL", "COM1", "com9.dat", "LPT1", "lpt9.x", "dir/CON.txt"} {
		mustErrCode(t, p, 0, CodeReservedName)
	}
	for _, p := range []string{"CONSOLE", "NULL", "COM10", "contact.txt"} {
		mustValid(t, p, 0, p)
	}
}

func TestValidateRelPath_TrailingDotSpace(t *testing.T) {
	for _, p := range []string{"file.", "dir /file.txt", "name "} {
		mustErrCode(t, p, 0, CodeTrailingDotSpace)
	}
}

func TestValidateRelPath_NULAndControl(t *testing.T) {
	mustErrCode(t, "a\x00b", 0, CodeNUL)
	mustErrCode(t, "a\x01b", 0, CodeForbiddenChar)
	mustErrCode(t, "a\x7fb", 0, CodeForbiddenChar)
}

func TestValidateRelPath_TooLong(t *testing.T) {
	p241 := strings.Repeat("a", 241)
	mustErrCode(t, p241, 0, CodeTooLong)
	mustValid(t, p241, 300, p241)
	seg256 := strings.Repeat("b", 256)
	mustErrCode(t, seg256, 3000, CodeTooLong)
}

func TestValidateRelPath_EmptyAndSegments(t *testing.T) {
	mustErrCode(t, "", 0, CodeEmpty)
	mustErrCode(t, "a//b", 0, CodeEmptySegment)
	mustErrCode(t, "a/b/", 0, CodeEmptySegment)
	mustErrCode(t, "a/./b", 0, CodeDotSegment)
	mustValid(t, ".", 0, ".")
}

func TestValidateRelPath_UnicodeNFCNormalization(t *testing.T) {
	// NFD: "e" + combining acute (U+0301); NFC: single rune é (U+00E9).
	nfd := "docs/e\u0301.txt"
	nfc := "docs/\u00e9.txt"
	mustValid(t, nfd, 0, nfc)
	mustValid(t, nfc, 0, nfc)
	// Vietnamese: "tiếng" in NFD form.
	nfdVi := "tie\u0302\u0301ng.txt"
	got, err := ValidateRelPath(nfdVi, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ti\u1ebfng.txt" {
		t.Fatalf("NFC normalization of Vietnamese = %q, want %q", got, "ti\u1ebfng.txt")
	}
}

func TestCheckCaseCollision(t *testing.T) {
	err := CheckCaseCollision([]string{"Readme.md", "README.MD"})
	if !HasCode(err, CodeCaseCollision) {
		t.Fatalf("case pair: err = %v", err)
	}
	// Same name in NFC vs NFD form.
	err = CheckCaseCollision([]string{"caf\u00e9.txt", "cafe\u0301.txt"})
	if !HasCode(err, CodeCaseCollision) {
		t.Fatalf("NFC/NFD pair: err = %v", err)
	}
	if err := CheckCaseCollision([]string{"a.txt", "B.txt"}); err != nil {
		t.Fatalf("distinct names: %v", err)
	}
	if err := CheckCaseCollision(nil); err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if err := CheckCaseCollision([]string{"only.txt"}); err != nil {
		t.Fatalf("single name: %v", err)
	}
}

func TestPathsCaseEqual(t *testing.T) {
	if !PathsCaseEqual("A/B.txt", "a/b.TXT") {
		t.Fatal("case-insensitive equal paths reported unequal")
	}
	if PathsCaseEqual("A/B.txt", "a/c.TXT") {
		t.Fatal("different paths reported equal")
	}
	if !PathsCaseEqual("caf\u00e9/x", "cafe\u0301/X") {
		t.Fatal("NFC/NFD paths reported unequal")
	}
	if PathsCaseEqual("a/b", "a/b/c") {
		t.Fatal("different depth reported equal")
	}
}

func TestValidateSegment_Basics(t *testing.T) {
	if err := ValidateSegment("normal-name_1.txt"); err != nil {
		t.Fatalf("valid segment: %v", err)
	}
	if err := ValidateSegment(""); !HasCode(err, CodeEmpty) {
		t.Fatalf("empty: %v", err)
	}
	if err := ValidateSegment("a/b"); !HasCode(err, CodeForbiddenChar) {
		t.Fatalf("slash: %v", err)
	}
	if err := ValidateSegment("con"); !HasCode(err, CodeReservedName) {
		t.Fatalf("reserved: %v", err)
	}
	if err := ValidateSegment("bad\x00name"); !HasCode(err, CodeNUL) {
		t.Fatalf("nul: %v", err)
	}
	// HasCode must be false for foreign errors.
	if HasCode(errFake{}, CodeEmpty) {
		t.Fatal("HasCode true for non-validate error")
	}
}

type errFake struct{}

func (errFake) Error() string { return "fake" }
