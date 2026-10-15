package main

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// partialArgs reads the string fields of a tool call's JSON arguments while
// the model is still writing them: a field shows up as soon as its value
// starts, and grows as more arrives. Only the top level and the objects of
// one array (edit's "edits") are read; numbers, booleans and nesting deeper
// than that are skipped. It never re-reads what it has already parsed, so a
// long write costs the same per chunk however much came before.
type partialArgs struct {
	buf []byte // everything received, unparsed from pos on
	pos int

	depth   int    // object/array nesting: 1 = top level object
	inArray bool   // inside the top-level array field arrayKey
	arrKey  string // that field's name
	key     string // the current key, once read
	wantKey bool   // the next string is a key
	inStr   bool   // inside a string
	isKey   bool   // that string is a key
	esc     bool   // after a backslash
	uesc    []byte // a \u escape so far
	high    rune   // a UTF-16 high surrogate waiting for its low half
	cur     strings.Builder

	fields map[string]string // top-level string fields, complete
	items  []map[string]string
	// growing is the field being read now ("" when none): its value so far
	// is in cur, read with value, so a long content isn't copied per chunk.
	growing     string
	growingItem int // its edits item, or -1 at the top level
}

func newPartialArgs() *partialArgs {
	return &partialArgs{fields: map[string]string{}}
}

// add parses the next chunk of the raw JSON.
func (p *partialArgs) add(chunk string) {
	p.buf = append(p.buf, chunk...)
	for p.pos < len(p.buf) {
		c := p.buf[p.pos]
		if p.inStr {
			if !p.string(c) {
				return // an escape or a rune cut off at the chunk's end
			}
			continue
		}
		p.pos++
		switch c {
		case '{':
			p.depth++
			p.wantKey = true
			if p.inArray && p.depth == 3 {
				p.items = append(p.items, map[string]string{})
			}
		case '}':
			p.depth--
			p.wantKey = false
		case '[':
			p.depth++
			if p.depth == 2 && p.key != "" {
				p.inArray, p.arrKey = true, p.key
			}
		case ']':
			if p.inArray && p.depth == 2 {
				p.inArray = false
			}
			p.depth--
		case ',':
			if p.depth == 1 || p.inArray && p.depth == 3 {
				p.wantKey = true
			}
		case ':':
			p.wantKey = false
		case '"':
			p.inStr, p.isKey = true, p.wantKey
			p.cur.Reset()
		}
	}
	p.growing = ""
	if p.inStr && !p.isKey && p.key != "" {
		switch {
		case p.depth == 1:
			p.growing, p.growingItem = p.key, -1
		case p.inArray && p.depth == 3 && len(p.items) > 0:
			p.growing, p.growingItem = p.key, len(p.items)-1
		}
	}
	// Keep only what's left to parse.
	if p.pos > 4096 {
		p.buf = append(p.buf[:0], p.buf[p.pos:]...)
		p.pos = 0
	}
}

// string consumes c inside a string; false when more bytes are needed.
func (p *partialArgs) string(c byte) bool {
	switch {
	case p.uesc != nil:
		p.uesc = append(p.uesc, c)
		p.pos++
		if len(p.uesc) == 4 {
			r, err := strconv.ParseUint(string(p.uesc), 16, 32)
			p.uesc = nil
			if err == nil {
				p.writeUnicode(rune(r))
			}
		}
	case p.esc:
		p.esc = false
		p.pos++
		switch c {
		case 'n':
			p.cur.WriteByte('\n')
		case 't':
			p.cur.WriteByte('\t')
		case 'r':
			p.cur.WriteByte('\r')
		case 'b', 'f':
		case 'u':
			p.uesc = []byte{}
		default:
			p.cur.WriteByte(c) // \" \\ \/
		}
	case c == '\\':
		p.esc = true
		p.pos++
	case c == '"':
		p.pos++
		p.inStr = false
		if p.isKey {
			p.key = p.cur.String()
			if p.depth == 1 && p.key != p.arrKey {
				p.inArray = false
			}
			return true
		}
		p.store()
	default:
		if c >= utf8.RuneSelf && !utf8.FullRune(p.buf[p.pos:]) {
			return false // the rest of this rune is in the next chunk
		}
		_, n := utf8.DecodeRune(p.buf[p.pos:])
		p.cur.Write(p.buf[p.pos : p.pos+n])
		p.pos += n
	}
	return true
}

func (p *partialArgs) writeUnicode(r rune) {
	switch {
	case utf16.IsSurrogate(r) && r < 0xdc00:
		p.high = r
		return
	case utf16.IsSurrogate(r) && p.high != 0:
		r = utf16.DecodeRune(p.high, r)
	}
	p.high = 0
	p.cur.WriteRune(r)
}

// store files the string just read (or read so far) under its key.
func (p *partialArgs) store() {
	if p.isKey || p.key == "" {
		return
	}
	switch {
	case p.depth == 1:
		p.fields[p.key] = p.cur.String()
	case p.inArray && p.depth == 3 && len(p.items) > 0:
		p.items[len(p.items)-1][p.key] = p.cur.String()
	}
}

// value is a top-level string field so far ("" when absent).
func (p *partialArgs) value(k string) string {
	if p.growing == k && p.growingItem < 0 {
		return p.cur.String()
	}
	return p.fields[k]
}

// itemValue is field k of edits item i so far.
func (p *partialArgs) itemValue(i int, k string) string {
	if p.growing == k && p.growingItem == i {
		return p.cur.String()
	}
	if i < len(p.items) {
		return p.items[i][k]
	}
	return ""
}

// tail is the last n lines of field k (top level, or of the last edits
// item when item is true) without copying the whole value: what a live box
// shows of a long content.
func (p *partialArgs) tail(k string, item bool, n int) (lines []string, total int) {
	var s string
	switch {
	case p.growing == k && (p.growingItem < 0) != item:
		s = p.cur.String() // Builder.String doesn't copy
	case item && len(p.items) > 0:
		s = p.items[len(p.items)-1][k]
	case !item:
		s = p.fields[k]
	}
	if strings.TrimSpace(s) == "" {
		return nil, 0
	}
	total = strings.Count(strings.TrimRight(s, " \t\n"), "\n") + 1 // a line just begun doesn't count yet
	end := len(s)
	for len(lines) < n && end > 0 {
		start := strings.LastIndexByte(s[:end], '\n') + 1
		if l := s[start:end]; len(lines) > 0 || strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
		end = start - 1
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, total
}
