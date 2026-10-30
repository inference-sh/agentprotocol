// Package tomlstr reads the string values of the TOML config files agents
// keep (kimi, codex), enough to look up a setting without a TOML library.
package tomlstr

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Read reads a TOML file as the string values it holds, keyed by Key. A
// missing file is an empty map.
func Read(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(string(raw)), nil
}

// Key joins a dotted TOML key's parts. Parts can hold dots themselves
// (providers."managed:kimi-code"), so they are joined with a byte TOML keys
// cannot hold.
func Key(parts ...string) string { return strings.Join(parts, "\x00") }

// Parse reads the string values of a TOML document: tables and inline
// tables, with bare and quoted keys. Other values (numbers, booleans,
// arrays) are skipped, as are arrays of tables. It is lenient: what it
// cannot read it skips to the end of the line.
func Parse(src string) map[string]string {
	p := &tomlParser{s: src, out: map[string]string{}}
	var table []string
	for {
		p.skipSpace(true)
		if p.eof() {
			return p.out
		}
		if p.s[p.i] == '[' {
			p.i++
			if !p.eof() && p.s[p.i] == '[' {
				// An array of tables: its values go under a path no
				// lookup names.
				p.i++
				table = append([]string{"[["}, p.keys()...)
			} else {
				table = p.keys()
			}
			p.skipLine()
			continue
		}
		if !p.keyValue(table) {
			p.skipLine()
		}
	}
}

type tomlParser struct {
	s   string
	i   int
	out map[string]string
}

func (p *tomlParser) eof() bool { return p.i >= len(p.s) }

// skipSpace skips blanks and comments, and newlines too when lines is set.
func (p *tomlParser) skipSpace(lines bool) {
	for !p.eof() {
		switch c := p.s[p.i]; {
		case c == ' ' || c == '\t':
			p.i++
		case lines && (c == '\n' || c == '\r'):
			p.i++
		case c == '#':
			for !p.eof() && p.s[p.i] != '\n' {
				p.i++
			}
		default:
			return
		}
	}
}

func (p *tomlParser) skipLine() {
	for !p.eof() && p.s[p.i] != '\n' {
		p.i++
	}
}

// keys reads a dotted key, up to and past a closing ']' or up to '='.
func (p *tomlParser) keys() []string {
	var ks []string
	for {
		p.skipSpace(false)
		if p.eof() {
			return ks
		}
		switch c := p.s[p.i]; c {
		case '"', '\'':
			s, ok := p.str()
			if !ok {
				return ks
			}
			ks = append(ks, s)
		default:
			start := p.i
			for !p.eof() && isBare(p.s[p.i]) {
				p.i++
			}
			if p.i == start {
				return ks
			}
			ks = append(ks, p.s[start:p.i])
		}
		p.skipSpace(false)
		if p.eof() {
			return ks
		}
		switch p.s[p.i] {
		case '.':
			p.i++
		case ']':
			p.i++
			if !p.eof() && p.s[p.i] == ']' {
				p.i++
			}
			return ks
		default:
			return ks
		}
	}
}

func isBare(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// keyValue reads "key = value" under table; false when it is not one.
func (p *tomlParser) keyValue(table []string) bool {
	ks := p.keys()
	p.skipSpace(false)
	if len(ks) == 0 || p.eof() || p.s[p.i] != '=' {
		return false
	}
	p.i++
	p.value(append(append([]string{}, table...), ks...), true)
	return true
}

// value reads a value, keeping it under path when it is a string and keep is
// set.
func (p *tomlParser) value(path []string, keep bool) {
	p.skipSpace(false)
	if p.eof() {
		return
	}
	switch c := p.s[p.i]; c {
	case '"', '\'':
		if s, ok := p.str(); ok && keep {
			p.out[Key(path...)] = s
		}
	case '{':
		p.i++
		for {
			p.skipSpace(false)
			if p.eof() || p.s[p.i] == '\n' {
				return
			}
			if p.s[p.i] == '}' {
				p.i++
				return
			}
			if !p.keyValue(path) {
				p.skipLine()
				return
			}
			p.skipSpace(false)
			if !p.eof() && p.s[p.i] == ',' {
				p.i++
			}
		}
	case '[':
		p.i++
		for {
			p.skipSpace(true)
			if p.eof() {
				return
			}
			if p.s[p.i] == ']' {
				p.i++
				return
			}
			before := p.i
			p.value(nil, false)
			p.skipSpace(true)
			if !p.eof() && p.s[p.i] == ',' {
				p.i++
			}
			if p.i == before {
				p.i++
			}
		}
	default:
		for !p.eof() && !strings.ContainsRune(" \t\r\n,}]#", rune(p.s[p.i])) {
			p.i++
		}
	}
}

// str reads a basic or literal string, single- or multi-line.
func (p *tomlParser) str() (string, bool) {
	q := p.s[p.i]
	if strings.HasPrefix(p.s[p.i:], strings.Repeat(string(q), 3)) {
		p.i += 3
		end := strings.Index(p.s[p.i:], strings.Repeat(string(q), 3))
		if end < 0 {
			p.i = len(p.s)
			return "", false
		}
		s := strings.TrimPrefix(p.s[p.i:p.i+end], "\n")
		p.i += end + 3
		return s, true
	}
	start := p.i
	p.i++
	for !p.eof() && p.s[p.i] != q && p.s[p.i] != '\n' {
		if q == '"' && p.s[p.i] == '\\' {
			p.i++
		}
		p.i++
	}
	if p.eof() || p.s[p.i] != q {
		return "", false
	}
	p.i++
	if q == '\'' {
		return p.s[start+1 : p.i-1], true
	}
	s, err := strconv.Unquote(p.s[start:p.i])
	if err != nil {
		return "", false
	}
	return s, true
}
