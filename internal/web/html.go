package web

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---- tokenizer ----

type tokKind uint8

const (
	tokText tokKind = iota
	tokStart
	tokEnd
)

type attr struct{ key, val string }

// token is one piece of an HTML document. Text tokens hold raw (still
// entity-encoded) text; tag names and attribute keys are lower case and
// attribute values are decoded.
type token struct {
	kind  tokKind
	name  string // tag name (start/end)
	text  string // text token content
	attrs []attr
	self  bool // <x/>
}

func (t *token) attr(key string) (string, bool) {
	for _, a := range t.attrs {
		if a.key == key {
			return a.val, true
		}
	}
	return "", false
}

// rawTextTags hold unparsed content up to their closing tag.
var rawTextTags = map[string]bool{"script": true, "style": true, "textarea": true, "title": true, "xmp": true, "noscript": true}

var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true, "input": true,
	"link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true,
}

// tokenize splits an HTML document into tokens. It never fails: malformed
// markup degrades to text. Comments, doctypes and processing instructions
// are dropped.
func tokenize(s string) []token {
	toks := make([]token, 0, len(s)/32)
	text := func(t string) {
		if t != "" {
			toks = append(toks, token{kind: tokText, text: t})
		}
	}
	i, start := 0, 0
	for i < len(s) {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		if i+1 >= len(s) {
			break
		}
		c := s[i+1]
		switch {
		case strings.HasPrefix(s[i:], "<!--"):
			text(s[start:i])
			end := strings.Index(s[i+4:], "-->")
			if end < 0 {
				return toks
			}
			i += 4 + end + 3
			start = i
		case c == '!' || c == '?':
			text(s[start:i])
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				return toks
			}
			i += end + 1
			start = i
		case c == '/' || isAlpha(c):
			tok, n := parseTag(s[i:])
			if n == 0 {
				i++
				continue
			}
			text(s[start:i])
			toks = append(toks, tok)
			i += n
			start = i
			if tok.kind == tokStart && !tok.self && rawTextTags[tok.name] {
				end := indexFold(s[i:], "</"+tok.name)
				if end < 0 {
					end = len(s) - i
				}
				text(s[i : i+end])
				i += end
				start = i
			}
		default:
			i++
		}
	}
	text(s[start:])
	return toks
}

func isAlpha(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// indexFold is a case-insensitive strings.Index for an ASCII needle.
func indexFold(s, sub string) int {
	n := len(sub)
	for i := range len(s) - n + 1 {
		if strings.EqualFold(s[i:i+n], sub) {
			return i
		}
	}
	return -1
}

// parseTag parses a tag at the start of s ("<a ...>" or "</a>") and returns
// it with its length; n is 0 when s does not hold a complete tag.
func parseTag(s string) (tok token, n int) {
	i := 1
	if s[i] == '/' {
		tok.kind = tokEnd
		i++
	} else {
		tok.kind = tokStart
	}
	j := i
	for j < len(s) && !isSpaceByte(s[j]) && s[j] != '>' && s[j] != '/' {
		j++
	}
	if j == i || j >= len(s) {
		return token{}, 0
	}
	tok.name = strings.ToLower(s[i:j])
	i = j
	for i < len(s) {
		for i < len(s) && (isSpaceByte(s[i]) || s[i] == '/') {
			if s[i] == '/' && i+1 < len(s) && s[i+1] == '>' {
				tok.self = true
			}
			i++
		}
		if i >= len(s) {
			return token{}, 0
		}
		if s[i] == '>' {
			if tok.kind == tokStart && voidTags[tok.name] {
				tok.self = true
			}
			return tok, i + 1
		}
		k := i
		for k < len(s) && !isSpaceByte(s[k]) && s[k] != '=' && s[k] != '>' && !(s[k] == '/' && k+1 < len(s) && s[k+1] == '>') {
			k++
		}
		key := strings.ToLower(s[i:k])
		i = k
		for i < len(s) && isSpaceByte(s[i]) {
			i++
		}
		val := ""
		if i < len(s) && s[i] == '=' {
			i++
			for i < len(s) && isSpaceByte(s[i]) {
				i++
			}
			if i < len(s) && (s[i] == '"' || s[i] == '\'') {
				q := s[i]
				end := strings.IndexByte(s[i+1:], q)
				if end < 0 {
					return token{}, 0
				}
				val = s[i+1 : i+1+end]
				i += end + 2
			} else {
				k := i
				for k < len(s) && !isSpaceByte(s[k]) && s[k] != '>' {
					k++
				}
				val = s[i:k]
				i = k
			}
		}
		if tok.kind == tokStart && key != "" && len(tok.attrs) < 32 {
			tok.attrs = append(tok.attrs, attr{key, decodeEntities(val)})
		}
	}
	return token{}, 0
}

// ---- entities ----

var namedEntities = map[string]string{
	"amp": "&", "lt": "<", "gt": ">", "quot": "\"", "apos": "'", "nbsp": " ",
	"copy": "©", "reg": "®", "trade": "™", "hellip": "…", "mdash": "—", "ndash": "–",
	"lsquo": "‘", "rsquo": "’", "sbquo": "‚", "ldquo": "“", "rdquo": "”", "bdquo": "„",
	"laquo": "«", "raquo": "»", "lsaquo": "‹", "rsaquo": "›", "bull": "•", "middot": "·",
	"deg": "°", "times": "×", "divide": "÷", "plusmn": "±", "minus": "−", "micro": "µ",
	"euro": "€", "pound": "£", "yen": "¥", "cent": "¢", "sect": "§", "para": "¶",
	"shy": "\u00ad", "zwj": "\u200d", "zwnj": "\u200c", "thinsp": "\u2009", "ensp": "\u2002", "emsp": "\u2003",
	"larr": "←", "rarr": "→", "uarr": "↑", "darr": "↓", "harr": "↔", "rArr": "⇒", "lArr": "⇐",
	"le": "≤", "ge": "≥", "ne": "≠", "asymp": "≈", "infin": "∞", "check": "✓", "dagger": "†",
	"frac12": "½", "frac14": "¼", "frac34": "¾", "sup2": "²", "sup3": "³", "iexcl": "¡", "iquest": "¿",
	"aacute": "á", "eacute": "é", "iacute": "í", "oacute": "ó", "uacute": "ú", "ntilde": "ñ",
	"auml": "ä", "ouml": "ö", "uuml": "ü", "Auml": "Ä", "Ouml": "Ö", "Uuml": "Ü", "szlig": "ß",
	"agrave": "à", "egrave": "è", "ccedil": "ç", "ecirc": "ê", "acirc": "â", "ocirc": "ô",
	"Eacute": "É", "aring": "å", "oslash": "ø", "aelig": "æ", "hyphen": "‐", "Tab": "\t", "NewLine": "\n",
}

// decodeEntities decodes numeric and common named character references.
// Unknown references are left as is.
func decodeEntities(s string) string {
	amp := strings.IndexByte(s, '&')
	if amp < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for amp >= 0 {
		b.WriteString(s[:amp])
		s = s[amp:]
		rep, n := decodeEntity(s)
		if n == 0 {
			b.WriteByte('&')
			s = s[1:]
		} else {
			b.WriteString(rep)
			s = s[n:]
		}
		amp = strings.IndexByte(s, '&')
	}
	b.WriteString(s)
	return b.String()
}

// decodeEntity decodes the reference at the start of s ("&...").
func decodeEntity(s string) (string, int) {
	if len(s) < 3 {
		return "", 0
	}
	if s[1] == '#' {
		i, base := 2, 10
		if s[i] == 'x' || s[i] == 'X' {
			i, base = 3, 16
		}
		j := i
		for j < len(s) && j-i < 8 && (isDigit(s[j]) || base == 16 && isHex(s[j])) {
			j++
		}
		if j == i {
			return "", 0
		}
		v, err := strconv.ParseUint(s[i:j], base, 32)
		if err != nil {
			return "", 0
		}
		if j < len(s) && s[j] == ';' {
			j++
		}
		r := rune(v)
		if r >= 0x80 && r <= 0x9f {
			r = cp1252[r-0x80]
		}
		if r == 0 || !utf8.ValidRune(r) {
			r = utf8.RuneError
		}
		return string(r), j
	}
	j := 1
	for j < len(s) && j < 12 && (isAlpha(s[j]) || isDigit(s[j])) {
		j++
	}
	name := s[1:j]
	if rep, ok := namedEntities[name]; ok {
		if j < len(s) && s[j] == ';' {
			return rep, j + 1
		}
		switch name { // legacy references valid without ';'
		case "amp", "lt", "gt", "quot", "nbsp", "copy", "reg":
			return rep, j
		}
	}
	return "", 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isHex(c byte) bool   { return isDigit(c) || c|0x20 >= 'a' && c|0x20 <= 'f' }

// ---- extraction ----

// dropTags are skipped with their content. header and footer are kept
// inside an <article>, where they usually hold the title and byline.
var dropTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true, "nav": true, "aside": true, "form": true,
	"template": true, "iframe": true, "object": true, "canvas": true, "button": true, "select": true,
	"textarea": true, "dialog": true, "title": true, "math": true,
}

var blockTags = map[string]bool{
	"div": true, "section": true, "article": true, "main": true, "header": true, "footer": true,
	"ul": true, "ol": true, "dl": true, "dt": true, "dd": true, "figure": true, "figcaption": true,
	"blockquote": true, "address": true, "details": true, "summary": true, "body": true, "center": true,
	"caption": true, "fieldset": true, "hgroup": true, "label": true,
}

type document struct {
	Title, Text string
}

// extract converts an HTML page to compact markdown-ish text. base resolves
// relative links (a <base href> overrides it).
func extract(src string, base *url.URL) document {
	toks := tokenize(src)
	var doc document
	var ogTitle string
	for i := range toks {
		t := &toks[i]
		if t.kind != tokStart {
			continue
		}
		switch t.name {
		case "title":
			if doc.Title == "" && i+1 < len(toks) && toks[i+1].kind == tokText {
				doc.Title = collapseSpace(decodeEntities(toks[i+1].text))
			}
		case "meta":
			if p, _ := t.attr("property"); p == "og:title" && ogTitle == "" {
				ogTitle, _ = t.attr("content")
				ogTitle = collapseSpace(ogTitle)
			}
		case "base":
			if h, ok := t.attr("href"); ok {
				if base == nil {
					if u, err := url.Parse(h); err == nil && u.IsAbs() {
						base = u
					}
				} else if u, err := base.Parse(h); err == nil {
					base = u
				}
			}
		}
	}
	if doc.Title == "" {
		doc.Title = ogTitle
	}

	from, to, inArticle := 0, len(toks), false
	if s, e := region(toks, "article", true); s >= 0 {
		from, to, inArticle = s, e, true
	} else if s, e := region(toks, "main", false); s >= 0 {
		from, to = s, e
	}
	text := render(toks[from:to], base, inArticle)
	if from != 0 || to != len(toks) {
		// A tiny article/main is probably a teaser box: use the whole page.
		if utf8.RuneCountInString(text) < 200 {
			if all := render(toks, base, false); utf8.RuneCountInString(all) > 4*utf8.RuneCountInString(text) {
				text = all
			}
		}
	}
	doc.Text = text
	return doc
}

// region returns the token range of the first <name> element (inclusive of
// its tags), or -1. With single set, it fails when the page has more than
// one such element (e.g. a list of article teasers).
func region(toks []token, name string, single bool) (int, int) {
	start, end, count := -1, -1, 0
	depth := 0
	for i := range toks {
		t := &toks[i]
		if t.name != name {
			continue
		}
		switch {
		case t.kind == tokStart && !t.self:
			if depth == 0 {
				count++
				if start < 0 {
					start = i
				}
			}
			depth++
		case t.kind == tokEnd && depth > 0:
			depth--
			if depth == 0 && end < 0 && start >= 0 {
				end = i + 1
			}
		}
	}
	if start < 0 || single && count > 1 {
		return -1, -1
	}
	if end < 0 {
		end = len(toks)
	}
	return start, end
}

func hiddenElement(t *token) bool {
	if _, ok := t.attr("hidden"); ok {
		return true
	}
	if v, _ := t.attr("aria-hidden"); v == "true" {
		return true
	}
	if v, _ := t.attr("style"); v != "" {
		v = strings.ReplaceAll(strings.ToLower(v), " ", "")
		return strings.Contains(v, "display:none") || strings.Contains(v, "visibility:hidden")
	}
	return false
}

type listState struct {
	ordered bool
	n       int
}

// openLink is an <a> whose closing tag is pending.
type openLink struct {
	start, at int // output length before the "[" (and its space), and after it; at < 0: plain text
	space     bool
	href      string
}

type renderer struct {
	buf       []byte
	glue      bool // next word attaches without a space (after "[" or "`")
	base      *url.URL
	space     bool // pending collapsed whitespace
	newlines  int  // trailing newlines already written
	pre       int
	lists     []listState
	links     []openLink
	codes     []openLink // open inline code spans
	cell      int        // >0 while inside a table cell
	rowCells  int
	rowHead   bool
	headerSep bool // separator row already written for this table
	tables    int
	lang      string // language of the <pre> being opened
}

func (r *renderer) blank() bool { return len(r.buf) == 0 }

// block ensures the output ends with at least n newlines (none at the start).
func (r *renderer) block(n int) {
	r.space = false
	if r.cell > 0 {
		r.space = true
		return
	}
	if r.blank() {
		return
	}
	for r.newlines < n {
		r.buf = append(r.buf, '\n')
		r.newlines++
	}
}

func (r *renderer) raw(s string) {
	if s == "" {
		return
	}
	r.buf = append(r.buf, s...)
	r.newlines = 0
	for i := len(s) - 1; i >= 0 && s[i] == '\n'; i-- {
		r.newlines++
	}
}

func (r *renderer) word(s string) {
	if r.space && r.newlines == 0 && !r.blank() && !r.glue {
		r.buf = append(r.buf, ' ')
	}
	r.space, r.glue = false, false
	r.raw(s)
}

// opener writes an opening "[" or "`"; the next word attaches to it.
func (r *renderer) opener(s string) {
	r.word(s)
	r.glue = true
}

// closer writes a closing mark directly after the last word, keeping any
// pending whitespace for the next one.
func (r *renderer) closer(s string) {
	sp := r.space
	r.space, r.glue = false, false
	r.raw(s)
	r.space = sp
}

// unwrap handles a mark (link bracket, backtick) whose element just closed:
// an empty element is removed entirely and one spanning lines loses its
// opening mark. It reports whether it did either; otherwise the caller
// writes the closing mark.
func (r *renderer) unwrap(l openLink) bool {
	inner := bytes.TrimSpace(r.buf[l.at:])
	switch {
	case len(inner) == 0:
		r.buf = r.buf[:l.start]
		r.space, r.glue = l.space || r.space, false
		r.newlines = 0
		for i := len(r.buf) - 1; i >= 0 && r.buf[i] == '\n'; i-- {
			r.newlines++
		}
	case bytes.IndexByte(inner, '\n') >= 0:
		r.buf = append(r.buf[:l.at-1], r.buf[l.at:]...)
	default:
		return false
	}
	return true
}

func (r *renderer) text(s string) {
	s = decodeEntities(s)
	if r.pre > 0 {
		r.raw(s)
		return
	}
	for i, f := range strings.Fields(s) {
		if i == 0 && !isSpaceByte(s[0]) && !strings.HasPrefix(s, "\u00a0") {
			// Continues the previous word unless whitespace was pending.
			r.word(f)
		} else {
			r.space = true
			r.word(f)
		}
	}
	if s != "" && strings.TrimRight(s, " \t\n\r\f\u00a0") != s {
		r.space = true
	}
}

func (r *renderer) resolve(href string) string {
	href = strings.TrimSpace(href)
	if r.base == nil {
		return href
	}
	u, err := r.base.Parse(href)
	if err != nil {
		return href
	}
	return u.String()
}

func render(toks []token, base *url.URL, inArticle bool) string {
	r := &renderer{base: base}
	for i := 0; i < len(toks); i++ {
		t := &toks[i]
		if t.kind == tokText {
			r.text(t.text)
			continue
		}
		name := t.name
		if t.kind == tokStart && !t.self && (dropTags[name] || !inArticle && (name == "header" || name == "footer") || hiddenElement(t)) {
			i = skipElement(toks, i)
			continue
		}
		if t.kind == tokStart {
			// The fence language comes from <pre>, its <code>, or a
			// wrapping <div class="highlight-source-go"> (GitHub).
			switch name {
			case "pre":
				if l := preLang(toks[i:]); l != "" {
					r.lang = l
				}
			case "div":
				r.lang = codeLang(t)
			}
			r.open(t)
		} else {
			r.close(name)
		}
	}
	return tidy(string(r.buf))
}

// skipElement returns the index of the end tag matching toks[i], or i when
// the element is never closed (only the tag itself is skipped).
func skipElement(toks []token, i int) int {
	name, depth := toks[i].name, 0
	for j := i; j < len(toks); j++ {
		t := &toks[j]
		if t.name != name {
			continue
		}
		if t.kind == tokStart && !t.self {
			depth++
		} else if t.kind == tokEnd {
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return i
}

func (r *renderer) open(t *token) {
	switch name := t.name; name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		r.block(2)
		if r.cell == 0 {
			r.raw(strings.Repeat("#", int(name[1]-'0')) + " ")
		}
	case "p":
		r.block(2)
	case "br":
		if r.pre > 0 {
			r.raw("\n")
		} else if r.cell > 0 {
			r.space = true
		} else {
			r.raw("\n")
			r.space = false
		}
	case "hr":
		r.block(2)
		r.raw("---")
		r.block(2)
	case "ul", "ol":
		r.block(1)
		r.lists = append(r.lists, listState{ordered: name == "ol"})
	case "li":
		r.block(1)
		depth := len(r.lists)
		if depth == 0 {
			depth = 1
		}
		marker := "- "
		if depth <= len(r.lists) && r.lists[depth-1].ordered {
			r.lists[depth-1].n++
			marker = strconv.Itoa(r.lists[depth-1].n) + ". "
		}
		if r.cell == 0 {
			r.raw(strings.Repeat("  ", depth-1) + marker)
		}
	case "pre":
		r.block(2)
		if r.pre == 0 && r.cell == 0 {
			r.raw("```" + r.lang + "\n")
			r.lang = ""
		}
		r.pre++
	case "code", "kbd", "samp", "tt":
		if r.pre == 0 {
			start, sp := len(r.buf), r.space
			r.opener("`")
			r.codes = append(r.codes, openLink{start: start, at: len(r.buf), space: sp})
		}
	case "a":
		href, _ := t.attr("href")
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(href)), "javascript:") || r.pre > 0 {
			r.links = append(r.links, openLink{at: -1})
			return
		}
		start, sp := len(r.buf), r.space
		r.opener("[")
		r.links = append(r.links, openLink{start: start, at: len(r.buf), space: sp, href: r.resolve(href)})
	case "img":
		alt, _ := t.attr("alt")
		alt = collapseSpace(alt)
		if alt != "" && r.pre == 0 {
			r.word("[image: " + alt + "]")
		}
	case "table":
		r.block(2)
		r.tables++
		r.headerSep = false
	case "tr":
		if r.tables == 0 {
			return
		}
		r.block(1)
		r.raw("|")
		r.rowCells, r.rowHead = 0, false
	case "td", "th":
		if r.tables == 0 {
			return
		}
		if name == "th" {
			r.rowHead = true
		}
		r.rowCells++
		r.cell++
		r.raw(" ")
		r.space = false
	default:
		if blockTags[name] {
			r.block(1)
		}
	}
}

func (r *renderer) close(name string) {
	switch name {
	case "h1", "h2", "h3", "h4", "h5", "h6", "p":
		r.block(2)
	case "ul", "ol":
		if n := len(r.lists); n > 0 {
			r.lists = r.lists[:n-1]
		}
		r.block(1)
		if len(r.lists) == 0 {
			r.block(2)
		}
	case "li":
		r.block(1)
	case "pre":
		if r.pre > 0 {
			r.pre--
			if r.pre == 0 && r.cell == 0 {
				if r.newlines == 0 {
					r.raw("\n")
				}
				r.raw("```")
				r.block(2)
			}
		}
	case "code", "kbd", "samp", "tt":
		if n := len(r.codes); r.pre == 0 && n > 0 {
			l := r.codes[n-1]
			r.codes = r.codes[:n-1]
			if !r.unwrap(l) {
				r.closer("`")
			}
		}
	case "a":
		n := len(r.links)
		if n == 0 {
			return
		}
		l := r.links[n-1]
		r.links = r.links[:n-1]
		if l.at >= 0 && !r.unwrap(l) {
			r.closer("](" + l.href + ")")
		}
	case "td", "th":
		if r.cell > 0 {
			r.cell--
			r.space = false
			r.raw(" |")
		}
	case "tr":
		if r.tables == 0 || r.rowCells == 0 {
			return
		}
		if r.rowHead && !r.headerSep {
			r.raw("\n|" + strings.Repeat(" --- |", r.rowCells))
			r.headerSep = true
		}
		r.block(1)
	case "table":
		if r.tables > 0 {
			r.tables--
		}
		r.block(2)
	default:
		if blockTags[name] {
			r.block(1)
		}
	}
}

// preLang finds the language of the <pre> at toks[0] from its class or a
// directly nested <code>.
func preLang(toks []token) string {
	if l := codeLang(&toks[0]); l != "" {
		return l
	}
	for i := 1; i < len(toks) && i < 4; i++ {
		t := &toks[i]
		switch {
		case t.kind == tokText && strings.TrimSpace(t.text) == "":
		case t.kind == tokStart && t.name == "code":
			return codeLang(t)
		default:
			return ""
		}
	}
	return ""
}

func codeLang(t *token) string {
	cls, _ := t.attr("class")
	for _, c := range strings.Fields(cls) {
		for _, p := range []string{"language-", "lang-", "highlight-source-"} {
			if strings.HasPrefix(c, p) {
				return c[len(p):]
			}
		}
	}
	return ""
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// tidy trims trailing spaces and collapses runs of blank lines (outside
// fenced code blocks only for trailing spaces, which code never needs).
func tidy(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	fence := false
	for _, l := range lines {
		if strings.HasPrefix(l, "```") {
			fence = !fence
		}
		l = strings.TrimRight(l, " \t\u00a0")
		if l == "" && !fence {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
