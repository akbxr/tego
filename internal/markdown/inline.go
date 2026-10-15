package markdown

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// style is a set of inline text attributes.
type style uint16

const (
	stBold style = 1 << iota
	stItalic
	stStrike
	stUnder
	stDim
	stCode   // inline code colour
	stAccent // heading colour
)

const (
	sgrReset  = "\x1b[0m"
	sgrDim    = "\x1b[2m"
	colAccent = "38;5;215"
	colCode   = "38;5;180"
	cMarker   = "\x1b[38;5;110m"
	cChecked  = "\x1b[38;5;114m"
	osc8Close = "\x1b]8;;\x1b\\"
)

// appendSGR appends the escape sequence selecting exactly the attributes in s
// (on top of a reset terminal state).
func appendSGR(dst []byte, s style) []byte {
	if s == 0 {
		return dst
	}
	dst = append(dst, "\x1b["...)
	n := len(dst)
	add := func(dst []byte, code string) []byte {
		if len(dst) > n {
			dst = append(dst, ';')
		}
		return append(dst, code...)
	}
	if s&stBold != 0 {
		dst = add(dst, "1")
	}
	if s&stDim != 0 {
		dst = add(dst, "2")
	}
	if s&stItalic != 0 {
		dst = add(dst, "3")
	}
	if s&stUnder != 0 {
		dst = add(dst, "4")
	}
	if s&stStrike != 0 {
		dst = add(dst, "9")
	}
	switch {
	case s&stCode != 0:
		dst = add(dst, colCode)
	case s&stAccent != 0:
		dst = add(dst, colAccent)
	}
	return append(dst, 'm')
}

// item is one run of inline text. Items with dch != 0 come from a delimiter
// run (*, _ or ~); n of their characters are still unmatched and printed.
type item struct {
	text string
	url  string // link target for OSC-8, "" outside links
	st   style

	dch         byte
	n, orig     int
	open, close bool
}

// parseInline appends the items of s to dst with emphasis resolved. st and url
// apply to every item (they are set when parsing link text).
func parseInline(dst []item, s string, st style, url string) []item {
	start := len(dst)
	text := 0 // start of pending literal text
	flush := func(dst []item, end int) []item {
		if end > text {
			dst = append(dst, item{text: s[text:end], st: st, url: url})
		}
		return dst
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '\\':
			if i+1 < len(s) && isASCIIPunct(s[i+1]) {
				dst = flush(dst, i)
				dst = append(dst, item{text: s[i+1 : i+2], st: st, url: url})
				i += 2
				text = i
				continue
			}
		case '`':
			n := runLen(s, i, '`')
			if j := closingTicks(s, i+n, n); j >= 0 {
				dst = flush(dst, i)
				code := s[i+n : j]
				if len(code) >= 2 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
					code = code[1 : len(code)-1]
				}
				dst = append(dst, item{text: code, st: st | stCode, url: url})
				i = j + n
				text = i
				continue
			}
			i += n
			continue
		case '*', '_', '~':
			n := runLen(s, i, c)
			if c == '~' && n > 2 {
				i += n
				continue
			}
			prev, next := ' ', ' '
			if i > 0 {
				prev, _ = utf8.DecodeLastRuneInString(s[:i])
			}
			if i+n < len(s) {
				next, _ = utf8.DecodeRuneInString(s[i+n:])
			}
			left := !unicode.IsSpace(next) && (!isPunct(next) || unicode.IsSpace(prev) || isPunct(prev))
			right := !unicode.IsSpace(prev) && (!isPunct(prev) || unicode.IsSpace(next) || isPunct(next))
			open, cls := left, right
			if c == '_' {
				open = left && (!right || isPunct(prev))
				cls = right && (!left || isPunct(next))
			}
			dst = flush(dst, i)
			dst = append(dst, item{text: s[i : i+n], st: st, url: url, dch: c, n: n, orig: n, open: open, close: cls})
			i += n
			text = i
			continue
		case '[', '!':
			b := i
			if c == '!' {
				if i+1 >= len(s) || s[i+1] != '[' {
					break
				}
				b++
			}
			if lt, dest, end, ok := parseLink(s, b); ok {
				dst = flush(dst, i)
				dst = parseInline(dst, lt, st|stUnder, dest)
				if dest != "" && dest != lt {
					dst = append(dst, item{text: " (" + dest + ")", st: st | stDim})
				}
				i = end
				text = i
				continue
			}
		case '<':
			if j := autolinkEnd(s, i); j > 0 {
				i = j
				continue
			}
		case 'h':
			if (i == 0 || isURLBoundary(s[i-1])) && (strings.HasPrefix(s[i:], "https://") || strings.HasPrefix(s[i:], "http://")) {
				i = bareURLEnd(s, i)
				continue
			}
		}
		i++
	}
	dst = flush(dst, len(s))
	sub := dst[start:]
	resolveEmphasis(sub)
	for k := range sub {
		sub[k].open, sub[k].close = false, false
	}
	return dst
}

// resolveEmphasis matches delimiter runs following CommonMark's
// "process emphasis" procedure (plus GFM strikethrough).
func resolveEmphasis(items []item) {
	for c := range items {
		cl := &items[c]
		if cl.dch == 0 || !cl.close {
			continue
		}
		for cl.n > 0 {
			o := -1
			for k := c - 1; k >= 0; k-- {
				op := &items[k]
				if op.dch != cl.dch || !op.open || op.n == 0 {
					continue
				}
				if cl.dch == '~' {
					if op.n != cl.n {
						continue
					}
				} else if (op.close || cl.open) && (op.orig+cl.orig)%3 == 0 && (op.orig%3 != 0 || cl.orig%3 != 0) {
					continue
				}
				o = k
				break
			}
			if o < 0 {
				break
			}
			op := &items[o]
			use, f := 1, stItalic
			switch {
			case cl.dch == '~':
				use, f = cl.n, stStrike
			case op.n >= 2 && cl.n >= 2:
				use, f = 2, stBold
			}
			for k := o + 1; k < c; k++ {
				items[k].st |= f
				items[k].open, items[k].close = false, false
			}
			op.n -= use
			cl.n -= use
		}
	}
}

// renderItems appends items styled on top of base and returns the display
// width of the text.
func renderItems(dst []byte, items []item, base style, color bool) ([]byte, int) {
	w := 0
	cur, curURL := style(0), ""
	for i := range items {
		it := &items[i]
		t := it.text
		if it.dch != 0 {
			t = t[:it.n]
		}
		if t == "" {
			continue
		}
		if color {
			if it.url != curURL {
				if curURL != "" {
					dst = append(dst, osc8Close...)
				}
				if it.url != "" {
					dst = append(dst, "\x1b]8;;"...)
					dst = append(dst, it.url...)
					dst = append(dst, "\x1b\\"...)
				}
				curURL = it.url
			}
			if eff := base | it.st; eff != cur {
				if cur != 0 {
					dst = append(dst, sgrReset...)
				}
				dst = appendSGR(dst, eff)
				cur = eff
			}
		}
		dst = append(dst, t...)
		w += textWidth(t)
	}
	if curURL != "" {
		dst = append(dst, osc8Close...)
	}
	if cur != 0 {
		dst = append(dst, sgrReset...)
	}
	return dst, w
}

// parseLink parses "[text](dest)" with s[i] == '['.
func parseLink(s string, i int) (text, dest string, end int, ok bool) {
	depth := 0
	j := i
	for ; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
			continue
		case '`':
			n := runLen(s, j, '`')
			if k := closingTicks(s, j+n, n); k >= 0 {
				j = k + n - 1
			} else {
				j += n - 1
			}
			continue
		case '[':
			depth++
		case ']':
			depth--
		}
		if depth == 0 {
			break
		}
	}
	if j+1 >= len(s) || s[j+1] != '(' {
		return "", "", 0, false
	}
	text = s[i+1 : j]
	k := j + 2
	for k < len(s) && s[k] == ' ' {
		k++
	}
	if k < len(s) && s[k] == '<' {
		e := strings.IndexByte(s[k:], '>')
		if e < 0 {
			return "", "", 0, false
		}
		dest = s[k+1 : k+e]
		k += e + 1
	} else {
		d, par := k, 0
		for ; k < len(s); k++ {
			c := s[k]
			if c == ' ' || c < 0x20 {
				break
			}
			if c == '\\' && k+1 < len(s) {
				k++
				continue
			}
			if c == '(' {
				par++
			} else if c == ')' {
				if par == 0 {
					break
				}
				par--
			}
		}
		dest = s[d:k]
	}
	for k < len(s) && s[k] == ' ' {
		k++
	}
	if k < len(s) && (s[k] == '"' || s[k] == '\'') {
		q := s[k]
		e := strings.IndexByte(s[k+1:], q)
		if e < 0 {
			return "", "", 0, false
		}
		k += e + 2
		for k < len(s) && s[k] == ' ' {
			k++
		}
	}
	if k >= len(s) || s[k] != ')' {
		return "", "", 0, false
	}
	return text, dest, k + 1, true
}

// autolinkEnd returns the end of "<scheme:...>" or "<user@host>" at s[i], or -1.
func autolinkEnd(s string, i int) int {
	e := strings.IndexByte(s[i+1:], '>')
	if e <= 0 {
		return -1
	}
	in := s[i+1 : i+1+e]
	if strings.ContainsAny(in, " <\t") {
		return -1
	}
	if c := strings.IndexByte(in, ':'); c >= 2 || strings.IndexByte(in, '@') > 0 {
		return i + e + 2
	}
	return -1
}

func isURLBoundary(c byte) bool {
	return c == ' ' || c == '\t' || c == '(' || c == '*' || c == '_' || c == '~' || c == '"' || c == '\''
}

// bareURLEnd returns the end of the URL starting at s[i], excluding trailing
// punctuation the way GFM's extended autolinks do.
func bareURLEnd(s string, i int) int {
	j := i
	for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != '<' {
		j++
	}
	for j > i {
		c := s[j-1]
		if strings.IndexByte("?!.,:*_~'\"", c) >= 0 {
			j--
			continue
		}
		if c == ')' && strings.Count(s[i:j], "(") < strings.Count(s[i:j], ")") {
			j--
			continue
		}
		break
	}
	return j
}

func runLen(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// closingTicks finds a run of exactly n backticks at or after from.
func closingTicks(s string, from, n int) int {
	for j := from; j < len(s); {
		k := strings.IndexByte(s[j:], '`')
		if k < 0 {
			return -1
		}
		j += k
		m := runLen(s, j, '`')
		if m == n {
			return j
		}
		j += m
	}
	return -1
}

func isASCIIPunct(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}

func isPunct(r rune) bool {
	if r < utf8.RuneSelf {
		return isASCIIPunct(byte(r))
	}
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}

// textWidth is the display width of plain text without escape sequences.
func textWidth(s string) int {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return runewidth.StringWidth(s)
		}
	}
	return len(s)
}

// visWidth is the number of columns raw text occupies when echoed, skipping
// escape sequences and expanding tabs.
func visWidth(b []byte) int {
	col := 0
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			i = skipEscape(b, i)
			continue
		case c == '\t':
			col = (col/8 + 1) * 8
		case c < 0x20 || c == 0x7f:
		case c < utf8.RuneSelf:
			col++
		default:
			r, n := utf8.DecodeRune(b[i:])
			col += runewidth.RuneWidth(r)
			i += n
			continue
		}
		i++
	}
	return col
}

// skipEscape returns the index after the escape sequence starting at b[i].
func skipEscape(b []byte, i int) int {
	if i+1 >= len(b) {
		return len(b)
	}
	switch b[i+1] {
	case '[':
		j := i + 2
		for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
			j++
		}
		return min(j+1, len(b))
	case ']':
		for j := i + 2; j < len(b); j++ {
			if b[j] == 7 {
				return j + 1
			}
			if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
				return j + 2
			}
		}
		return len(b)
	}
	return i + 2
}
