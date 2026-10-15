package images

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// PathMatch is an image path found in a prompt: text[Start:End] (including
// any quotes or escapes) refers to the existing file Path (absolute, clean).
type PathMatch struct {
	Start, End int
	Path       string
}

// maxJoin bounds how many preceding words FindPaths tries prepending when a
// word ending in an image extension is not a file (an unescaped path with
// spaces, e.g. "/tmp/Screen Shot 1.png").
const maxJoin = 8

// FindPaths finds references to existing image files (png, jpg, jpeg, gif,
// webp) in text: bare or ~/ paths, paths relative to cwd, 'single' or
// "double" quoted paths, backslash-escaped spaces as terminals paste dragged
// files, and file:// URLs. Matches are ordered and never overlap.
func FindPaths(text, cwd string) []PathMatch {
	var out []PathMatch
	var words []int // starts of the last unquoted words since the last match
	add := func(start, end int, p string) {
		out = append(out, PathMatch{Start: start, End: end, Path: p})
		words = words[:0]
	}
	for i := 0; i < len(text); {
		c := text[i]
		if isSpace(c) {
			i++
			continue
		}
		if c == '\'' || c == '"' {
			if j := strings.IndexByte(text[i+1:], c); j >= 0 {
				end := i + 2 + j
				if inner := text[i+1 : end-1]; hasImageExt(inner) {
					if p, ok := resolve(inner, cwd, false); ok {
						add(i, end, p)
						i = end
						continue
					}
				}
			}
			i++ // not a quoted path: scan the rest as ordinary words
			continue
		}
		// Unquoted word: runs to unescaped whitespace.
		j := i
		for j < len(text) && !isSpace(text[j]) {
			if text[j] == '\\' && j+1 < len(text) {
				j++
			}
			j++
		}
		if len(words) > maxJoin {
			words = append(words[:0], words[1:]...)
		}
		words = append(words, i)
		start, end := trimWord(text, i, j)
		if start < end && hasImageExt(text[start:end]) {
			if p, ok := resolve(text[start:end], cwd, true); ok {
				add(start, end, p)
			} else {
				// Retry with preceding words for paths containing plain spaces.
				for k := len(words) - 2; k >= 0; k-- {
					s, _ := trimWord(text, words[k], end)
					if p, ok := resolve(text[s:end], cwd, true); ok {
						add(s, end, p)
						break
					}
				}
			}
		}
		i = j
	}
	return out
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// trimWord strips surrounding punctuation from text[i:j] (e.g. "(a.png),").
func trimWord(text string, i, j int) (int, int) {
	for i < j && strings.IndexByte("([{<'\"", text[i]) >= 0 {
		i++
	}
	for j > i && strings.IndexByte(")]}>,;:!?.'\"", text[j-1]) >= 0 && !hasImageExt(text[i:j]) {
		j--
	}
	return i, j
}

func hasImageExt(s string) bool {
	dot := strings.LastIndexByte(s, '.')
	if dot < 0 {
		return false
	}
	switch strings.ToLower(s[dot+1:]) {
	case "png", "jpg", "jpeg", "gif", "webp":
		return true
	}
	return false
}

// resolve turns a path reference into an absolute path when it names an
// existing regular file. unescape removes shell backslash escapes.
func resolve(s, cwd string, unescape bool) (string, bool) {
	if rest, ok := strings.CutPrefix(s, "file://"); ok {
		rest = strings.TrimPrefix(rest, "localhost")
		p, err := url.PathUnescape(rest)
		if err != nil || !strings.HasPrefix(p, "/") {
			return "", false
		}
		s, unescape = p, false
	}
	if unescape && strings.IndexByte(s, '\\') >= 0 {
		var b strings.Builder
		b.Grow(len(s))
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			b.WriteByte(s[i])
		}
		s = b.String()
	}
	switch {
	case s == "~" || strings.HasPrefix(s, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		s = filepath.Join(home, s[1:])
	case !filepath.IsAbs(s):
		s = filepath.Join(cwd, s)
	default:
		s = filepath.Clean(s)
	}
	st, err := os.Stat(s)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return s, true
}
