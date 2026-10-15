package tools

import (
	"errors"
	"path"
	"strings"
)

// globMatcher matches slash-separated relative paths against a glob with
// `**`, `*`, `?`, `[...]` (also `[!...]`) and `{a,b}` support. A pattern
// without a slash matches the base name at any depth (gitignore/rg style).
type globMatcher struct {
	pats []globPat
}

type globPat struct {
	segs []string
	base bool
}

const maxBraceExpansions = 256

func compileGlob(pattern string) (*globMatcher, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, errors.New("empty glob pattern")
	}
	exp, err := expandBraces(pattern)
	if err != nil {
		return nil, err
	}
	g := &globMatcher{}
	for _, p := range exp {
		for strings.HasPrefix(p, "./") {
			p = p[2:]
		}
		p = strings.TrimPrefix(p, "/")
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			continue
		}
		segs := strings.Split(p, "/")
		for i, s := range segs {
			s = strings.ReplaceAll(s, "[!", "[^")
			if _, err := path.Match(s, ""); err != nil {
				return nil, errors.New("bad glob pattern: " + pattern)
			}
			segs[i] = s
		}
		g.pats = append(g.pats, globPat{segs: segs, base: len(segs) == 1 && segs[0] != "**"})
	}
	if len(g.pats) == 0 {
		return nil, errors.New("bad glob pattern: " + pattern)
	}
	return g, nil
}

// Match reports whether rel (slash-separated, relative) matches.
func (g *globMatcher) Match(rel string) bool {
	var parts []string
	for _, p := range g.pats {
		if p.base {
			if ok, _ := path.Match(p.segs[0], path.Base(rel)); ok {
				return true
			}
			continue
		}
		if parts == nil {
			parts = strings.Split(rel, "/")
		}
		if matchSegs(p.segs, parts) {
			return true
		}
	}
	return false
}

func matchSegs(p, s []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			for len(p) > 0 && p[0] == "**" {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := range s {
				if matchSegs(p, s[i:]) {
					return true
				}
			}
			return false
		}
		if len(s) == 0 {
			return false
		}
		if ok, _ := path.Match(p[0], s[0]); !ok {
			return false
		}
		p, s = p[1:], s[1:]
	}
	return len(s) == 0
}

// expandBraces expands {a,b} alternations (nested allowed).
func expandBraces(p string) ([]string, error) {
	open, depth, commas := -1, 0, false
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '\\':
			i++
		case '{':
			if depth == 0 {
				open, commas = i, false
			}
			depth++
		case ',':
			if depth == 1 {
				commas = true
			}
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth > 0 {
				continue
			}
			if !commas {
				open = -1
				continue
			}
			pre, body, post := p[:open], p[open+1:i], p[i+1:]
			var out []string
			for _, alt := range splitTopLevel(body) {
				rest, err := expandBraces(pre + alt + post)
				if err != nil {
					return nil, err
				}
				out = append(out, rest...)
				if len(out) > maxBraceExpansions {
					return nil, errors.New("glob brace expansion too large")
				}
			}
			return out, nil
		}
	}
	return []string{p}, nil
}

func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
