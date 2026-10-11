// Package validate checks and normalizes peer-supplied relative paths so
// they are safe on Linux, Windows, and macOS before they ever touch the
// filesystem (threat model #2: path traversal by a malicious/buggy peer).
//
// Callers must use only the path returned by ValidateRelPath, never the raw
// peer-supplied string.
package validate

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Code classifies a validation failure so callers can react per file.
type Code string

const (
	CodeEmpty            Code = "empty"
	CodeAbsolute         Code = "absolute"
	CodeTraversal        Code = "traversal"
	CodeNUL              Code = "nul"
	CodeForbiddenChar    Code = "forbidden_char"
	CodeReservedName     Code = "reserved_name"
	CodeTrailingDotSpace Code = "trailing_dot_space"
	CodeDotSegment       Code = "dot_segment"
	CodeEmptySegment     Code = "empty_segment"
	CodeTooLong          Code = "too_long"
	CodeCaseCollision    Code = "case_collision"
)

// DefaultMaxLen is used when ValidateRelPath gets maxLen <= 0.
const DefaultMaxLen = 240

// Error is a validation failure with a machine-readable Code.
type Error struct {
	Code Code
	Path string // raw offending path, for caller-side logging only
	Msg  string
}

// Error returns "validate: <code>: <msg>". The raw path is never embedded in
// Msg by this package, so control characters in a hostile path cannot leak
// into logs through the error string.
func (e *Error) Error() string {
	return fmt.Sprintf("validate: %s: %s", e.Code, e.Msg)
}

func newError(code Code, path, msg string) *Error {
	return &Error{Code: code, Path: path, Msg: msg}
}

// HasCode reports whether err is a *Error with the given code.
func HasCode(err error, code Code) bool {
	var ve *Error
	if errors.As(err, &ve) {
		return ve.Code == code
	}
	return false
}

func hasControl(s string) (found bool, isNUL bool) {
	for _, r := range s {
		if r == 0 {
			return true, true
		}
		if r < 0x20 || r == 0x7F {
			return true, false
		}
	}
	return false, false
}

func isDrivePrefix(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// ValidateRelPath validates and normalizes a peer-supplied relative path.
// The returned clean path is NFC-normalized and "/" separated with no
// leading/trailing slash. maxLen <= 0 means DefaultMaxLen; it counts runes
// of the whole normalized path.
func ValidateRelPath(p string, maxLen int) (string, error) {
	if maxLen <= 0 {
		maxLen = DefaultMaxLen
	}
	// 1. empty
	if p == "" {
		return "", newError(CodeEmpty, p, "path is empty")
	}
	// 2. NUL and control characters (checked on the raw input)
	if ctrl, isNUL := hasControl(p); ctrl {
		if isNUL {
			return "", newError(CodeNUL, p, "path contains a NUL byte")
		}
		return "", newError(CodeForbiddenChar, p, "path contains a control character")
	}
	// 3. Unicode NFC normalization (stdlib has no NFC; see spec ADR note)
	p = norm.NFC.String(p)
	// 4. absolute paths: leading slash/backslash, drive letter, UNC
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || isDrivePrefix(p) {
		return "", newError(CodeAbsolute, p, "path is absolute")
	}
	// 5. backslash anywhere else: protocol paths use '/' only
	if strings.Contains(p, `\`) {
		return "", newError(CodeForbiddenChar, p, `path contains '\'`)
	}
	// 6. segment structure; "." alone denotes the sync root
	if p == "." {
		return ".", nil
	}
	segments := strings.Split(p, "/")
	for _, seg := range segments {
		switch {
		case seg == "":
			return "", newError(CodeEmptySegment, p, "path contains an empty segment")
		case seg == "..":
			return "", newError(CodeTraversal, p, "path contains a '..' segment")
		case seg == ".":
			return "", newError(CodeDotSegment, p, "path contains a '.' segment")
		}
	}
	// 7. per-segment checks
	for _, seg := range segments {
		if err := checkSegment(seg); err != nil {
			ve := err.(*Error)
			ve.Path = p
			return "", ve
		}
	}
	// 8. total length in runes, separators included
	if utf8.RuneCountInString(p) > maxLen {
		return "", newError(CodeTooLong, p, fmt.Sprintf("path exceeds %d runes", maxLen))
	}
	return p, nil
}

// ValidateSegment validates a single path component (no '/').
func ValidateSegment(name string) error {
	if err := checkSegment(name); err != nil {
		ve := err.(*Error)
		ve.Path = name
		return ve
	}
	return nil
}

// checkSegment implements the checks for one segment: empty, NUL/control,
// separators, Windows-forbidden chars, trailing dot/space, reserved names,
// and the 255-byte segment limit.
func checkSegment(seg string) error {
	if seg == "" {
		return newError(CodeEmpty, seg, "name is empty")
	}
	if ctrl, isNUL := hasControl(seg); ctrl {
		if isNUL {
			return newError(CodeNUL, seg, "name contains a NUL byte")
		}
		return newError(CodeForbiddenChar, seg, "name contains a control character")
	}
	if strings.ContainsAny(seg, `/\`) {
		return newError(CodeForbiddenChar, seg, "name contains a path separator")
	}
	if strings.ContainsAny(seg, `:*?"<>|`) {
		return newError(CodeForbiddenChar, seg, "name contains a character forbidden on Windows")
	}
	if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return newError(CodeTrailingDotSpace, seg, "name ends with '.' or a space")
	}
	stem := seg
	if i := strings.Index(seg, "."); i >= 0 {
		stem = seg[:i]
	}
	if _, reserved := reservedNames[asciiUpper(stem)]; reserved {
		return newError(CodeReservedName, seg, "name is a reserved device name on Windows")
	}
	if len(seg) > 255 {
		return newError(CodeTooLong, seg, "name exceeds 255 bytes")
	}
	return nil
}

func asciiUpper(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return unicode.ToUpper(r)
		}
		return r
	}, s)
}
