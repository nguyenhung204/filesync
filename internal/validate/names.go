package validate

import (
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// reservedNames holds the Windows device names, rejected on every platform
// because a file may later sync to a Windows machine. Built from a fixed
// list rather than a chain of conditionals.
var reservedNames = func() map[string]struct{} {
	set := map[string]struct{}{}
	for _, name := range []string{"CON", "PRN", "AUX", "NUL"} {
		set[name] = struct{}{}
	}
	for i := 1; i <= 9; i++ {
		set[fmt.Sprintf("COM%d", i)] = struct{}{}
		set[fmt.Sprintf("LPT%d", i)] = struct{}{}
	}
	return set
}()

// foldName is the canonical comparison form of a name: NFC, case-folded.
func foldName(s string) string {
	return strings.ToLower(norm.NFC.String(s))
}

// CheckCaseCollision reports a CodeCaseCollision error if any two names in
// the same directory are equal after NFC normalization and case folding —
// including pairs whose raw bytes differ only by case or by NFC vs NFD form.
func CheckCaseCollision(names []string) error {
	seen := map[string]string{} // folded -> first original
	for _, name := range names {
		folded := foldName(name)
		if prev, ok := seen[folded]; ok {
			return &Error{
				Code: CodeCaseCollision,
				Path: name,
				Msg:  fmt.Sprintf("names %q and %q collide ignoring case/unicode form", prev, name),
			}
		}
		seen[folded] = name
	}
	return nil
}

// PathsCaseEqual reports whether two paths are equal segment-by-segment
// under NFC normalization and case folding.
func PathsCaseEqual(a, b string) bool {
	as := strings.Split(norm.NFC.String(a), "/")
	bs := strings.Split(norm.NFC.String(b), "/")
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if strings.ToLower(as[i]) != strings.ToLower(bs[i]) {
			return false
		}
	}
	return true
}
