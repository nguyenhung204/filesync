// Package ignore parses and matches .syncignore files using .gitignore
// syntax: comments, negation, dir-only patterns, root anchoring, per-segment
// globs, and ** wildcards. Last matching pattern wins.
package ignore

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

type pattern struct {
	segments []string
	negated  bool
	dirOnly  bool
	anchored bool
}

// Matcher is a compiled set of ignore patterns.
type Matcher struct {
	patterns []pattern
}

// Parse reads patterns from r. A malformed glob returns an error naming the
// 1-based line number.
func Parse(r io.Reader) (*Matcher, error) {
	m := &Matcher{}
	reader := bufio.NewReader(r)
	lineNo := 0
	for {
		raw, err := reader.ReadString('\n')
		if raw != "" {
			lineNo++
			line := strings.TrimSuffix(raw, "\n")
			line = strings.TrimSuffix(line, "\r")
			p, keep, perr := parseLine(line)
			if perr != nil {
				return nil, fmt.Errorf("ignore: line %d: %w", lineNo, perr)
			}
			if keep {
				m.patterns = append(m.patterns, p)
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("ignore: read patterns: %w", err)
		}
	}
	return m, nil
}

// ParseString parses patterns from a string.
func ParseString(s string) (*Matcher, error) {
	return Parse(strings.NewReader(s))
}

// LoadFile loads patterns from path. A missing file yields an empty matcher
// and a nil error; other read errors are returned wrapped.
func LoadFile(path string) (*Matcher, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Matcher{}, nil
		}
		return nil, fmt.Errorf("ignore: open %s: %w", path, err)
	}
	defer f.Close()
	m, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("ignore: load %s: %w", path, err)
	}
	return m, nil
}

func parseLine(line string) (pattern, bool, error) {
	var p pattern
	if line == "" {
		return p, false, nil
	}
	switch {
	case strings.HasPrefix(line, `\#`):
		line = line[1:]
	case strings.HasPrefix(line, "#"):
		return p, false, nil
	}
	switch {
	case strings.HasPrefix(line, `\!`):
		line = line[1:]
	case strings.HasPrefix(line, "!"):
		p.negated = true
		line = line[1:]
	}
	line = trimTrailingSpaces(line)
	if line == "" {
		return p, false, nil
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if line == "" {
		return p, false, nil
	}
	if strings.Contains(line, "/") {
		p.anchored = true
		line = strings.TrimPrefix(line, "/")
	}
	p.segments = strings.Split(line, "/")
	for _, seg := range p.segments {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return p, false, err
		}
	}
	return p, true, nil
}

// trimTrailingSpaces removes trailing spaces, except a space escaped as
// "\ " at end of line: the backslash is dropped and the space kept.
func trimTrailingSpaces(s string) string {
	if strings.HasSuffix(s, `\ `) {
		return s[:len(s)-2] + " "
	}
	return strings.TrimRight(s, " ")
}
