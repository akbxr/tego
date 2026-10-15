package markdown

import (
	"strings"
	"sync"
)

const (
	hlKeyword = "\x1b[38;5;176m"
	hlString  = "\x1b[38;5;150m"
	hlNumber  = "\x1b[38;5;173m"
	hlComment = "\x1b[38;5;244m"
	hlFunc    = "\x1b[38;5;111m"
	hlType    = "\x1b[38;5;116m"
	hlKey     = "\x1b[38;5;117m"
	hlAdd     = "\x1b[38;5;114m"
	hlDel     = "\x1b[38;5;174m"
	hlHunk    = "\x1b[38;5;110m"
)

// Keyword classes.
const (
	kwKeyword = 1 + iota
	kwConst
	kwType
)

// lang describes a language family for the generic highlighter.
type lang struct {
	line          []string // line comment prefixes
	bOpen, bClose string   // block comment delimiters
	quotes        string   // string delimiters
	multi         string   // delimiters whose strings may span lines
	rawTick       bool     // backtick strings have no escapes (Go)
	charQ         bool     // ' starts a char literal, not a string
	triple        bool     // """ / ''' strings (Python)
	wordComment   bool     // line comments only at the start of a word (sh, yaml)
	vars          bool     // $VAR / ${...}
	directive     bool     // #include style directives
	keys          byte     // ':' (yaml) or '=' (toml) keys at line start
	jsonKeys      bool     // "key": strings
	fold          bool     // case-insensitive keywords (SQL)
	diff          bool
	kw            map[string]byte
}

// hlState carries constructs that span lines inside one code block.
type hlState struct {
	lang   *lang
	block  bool // inside a block comment
	str    byte // inside a multi-line string with this delimiter
	triple bool
}

var (
	langsOnce sync.Once
	langs     map[string]*lang
)

func words(m map[string]byte, kind byte, list string) {
	for _, w := range strings.Fields(list) {
		m[w] = kind
	}
}

func newLang(l lang, kw, consts, types string) *lang {
	l.kw = make(map[string]byte)
	words(l.kw, kwKeyword, kw)
	words(l.kw, kwConst, consts)
	words(l.kw, kwType, types)
	return &l
}

func initLangs() {
	cLike := []string{"//"}
	goL := newLang(lang{line: cLike, bOpen: "/*", bClose: "*/", quotes: "\"'`", multi: "`", rawTick: true, charQ: true},
		"break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var",
		"true false nil iota",
		"bool byte complex64 complex128 error float32 float64 int int8 int16 int32 int64 rune string uint uint8 uint16 uint32 uint64 uintptr any comparable append cap clear close copy delete len make max min new panic print println recover")
	rust := newLang(lang{line: cLike, bOpen: "/*", bClose: "*/", quotes: "\"'", charQ: true},
		"as async await break const continue crate dyn else enum extern fn for if impl in let loop match mod move mut pub ref return self static struct super trait type unsafe use where while",
		"true false None Some Ok Err Self",
		"i8 i16 i32 i64 i128 isize u8 u16 u32 u64 u128 usize f32 f64 bool char str String Vec Option Result Box")
	const jsKw = "break case catch class const continue debugger default delete do else export extends finally for function if import in instanceof let new return super switch this throw try typeof var void while with yield async await of static get set from as"
	js := newLang(lang{line: cLike, bOpen: "/*", bClose: "*/", quotes: "\"'`", multi: "`"},
		jsKw, "true false null undefined NaN Infinity", "console require module exports")
	ts := newLang(lang{line: cLike, bOpen: "/*", bClose: "*/", quotes: "\"'`", multi: "`"},
		jsKw+" interface type enum implements namespace declare readonly private public protected abstract keyof infer is satisfies",
		"true false null undefined NaN Infinity",
		"string number boolean any unknown never object bigint symbol console")
	py := newLang(lang{line: []string{"#"}, quotes: "\"'", triple: true},
		"and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield match case",
		"True False None self cls",
		"print len range int str list dict set tuple float bool bytes type isinstance super open enumerate zip map filter sorted object Exception")
	lua := newLang(lang{line: []string{"--"}, bOpen: "--[[", bClose: "]]", quotes: "\"'"},
		"and break do else elseif end for function goto if in local not or repeat return then until while continue export type",
		"true false nil self",
		"print pairs ipairs require tostring tonumber typeof table string math game workspace script")
	sh := newLang(lang{line: []string{"#"}, quotes: "\"'", wordComment: true, vars: true},
		"if then else elif fi case esac for while until do done in function return local export readonly declare unset shift exit break continue select time",
		"true false",
		"echo cd printf read source eval exec set test trap alias")
	json := newLang(lang{quotes: "\"", jsonKeys: true}, "", "true false null", "")
	yaml := newLang(lang{line: []string{"#"}, quotes: "\"'", wordComment: true, keys: ':'}, "", "true false null yes no on off True False", "")
	toml := newLang(lang{line: []string{"#", ";"}, quotes: "\"'", wordComment: true, keys: '='}, "", "true false", "")
	c := newLang(lang{line: cLike, bOpen: "/*", bClose: "*/", quotes: "\"'", charQ: true, directive: true},
		"auto break case const continue default do else enum extern for goto if inline register restrict return sizeof static struct switch typedef union volatile while class namespace template typename public private protected virtual override new delete this throw try catch using operator friend constexpr explicit noexcept static_cast dynamic_cast reinterpret_cast const_cast",
		"true false NULL nullptr",
		"char double float int long short signed unsigned void bool size_t int8_t int16_t int32_t int64_t uint8_t uint16_t uint32_t uint64_t std string")
	java := newLang(lang{line: cLike, bOpen: "/*", bClose: "*/", quotes: "\"'", charQ: true},
		"abstract assert break case catch class const continue default do else enum extends final finally for goto if implements import instanceof interface native new package private protected public return static strictfp super switch synchronized this throw throws transient try volatile while var val fun when object companion data sealed override open internal lateinit by is in out typealias suspend func struct protocol extension guard defer where using namespace readonly record async await yield let init self",
		"true false null nil",
		"boolean byte char double float int long short void String Int Long Double Float Boolean Unit Any List Map Set Array object string bool")
	sql := newLang(lang{line: []string{"--"}, bOpen: "/*", bClose: "*/", quotes: "'\"", fold: true},
		"select from where insert into update delete create table drop alter join left right inner outer full cross on group by order having limit offset union all and or not is in as distinct values set primary key foreign references index view begin commit rollback with case when then else end returning exists between like asc desc default constraint unique",
		"null true false",
		"int integer bigint text varchar char boolean date timestamp numeric real serial")
	diff := &lang{diff: true}

	langs = make(map[string]*lang)
	reg := func(l *lang, names string) {
		for _, n := range strings.Fields(names) {
			langs[n] = l
		}
	}
	reg(goL, "go golang")
	reg(rust, "rust rs")
	reg(js, "js javascript jsx mjs cjs node")
	reg(ts, "ts typescript tsx mts")
	reg(py, "py python python3 py3")
	reg(lua, "lua luau")
	reg(sh, "sh bash zsh shell console fish ksh shellscript")
	reg(json, "json jsonc json5 jsonl")
	reg(yaml, "yaml yml")
	reg(toml, "toml ini cfg conf")
	reg(c, "c h cpp c++ cc cxx hpp hh objc objective-c")
	reg(java, "java kotlin kt kts cs csharp c# swift scala dart groovy")
	reg(sql, "sql postgres postgresql mysql sqlite")
	reg(diff, "diff patch")
}

// lookupLang returns the highlighter for a fence info word, or nil.
func lookupLang(name string) *lang {
	if name == "" {
		return nil
	}
	langsOnce.Do(initLangs)
	if l := langs[name]; l != nil {
		return l
	}
	return langs[strings.ToLower(name)]
}

func tok(dst []byte, color, text string) []byte {
	if text == "" {
		return dst
	}
	dst = append(dst, color...)
	dst = append(dst, text...)
	return append(dst, sgrReset...)
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdent(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// nextNonSpace returns the first non-blank byte at or after i, or 0.
func nextNonSpace(s string, i int) byte {
	for ; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			return s[i]
		}
	}
	return 0
}

// scanStr scans a string body starting at i (after the opening delimiter)
// and returns the index after the closing delimiter.
func (h *hlState) scanStr(s string, i int, q byte, triple bool) (int, bool) {
	raw := q == '`' && h.lang.rawTick
	for i < len(s) {
		c := s[i]
		if c == '\\' && !raw {
			i += 2
			continue
		}
		if c == q {
			if !triple {
				return i + 1, true
			}
			if i+2 < len(s) && s[i+1] == q && s[i+2] == q {
				return i + 3, true
			}
		}
		i++
	}
	return len(s), false
}

// line appends s highlighted, updating multi-line state.
func (h *hlState) line(dst []byte, s string) []byte {
	l := h.lang
	if l.diff {
		switch {
		case strings.HasPrefix(s, "@@"):
			return tok(dst, hlHunk, s)
		case strings.HasPrefix(s, "+"):
			return tok(dst, hlAdd, s)
		case strings.HasPrefix(s, "-"):
			return tok(dst, hlDel, s)
		}
		return append(dst, s...)
	}
	i := 0
	if h.block {
		j := strings.Index(s, l.bClose)
		if j < 0 {
			return tok(dst, hlComment, s)
		}
		i = j + len(l.bClose)
		dst = tok(dst, hlComment, s[:i])
		h.block = false
	}
	if h.str != 0 {
		end, closed := h.scanStr(s, i, h.str, h.triple)
		dst = tok(dst, hlString, s[i:end])
		if !closed {
			return dst
		}
		h.str = 0
		i = end
	}
	if l.keys != 0 && i == 0 {
		dst, i = h.key(dst, s)
	}
	plain := i
	n := len(s)
	for i < n {
		c := s[i]
		if l.bOpen != "" && c == l.bOpen[0] && strings.HasPrefix(s[i:], l.bOpen) {
			dst = append(dst, s[plain:i]...)
			j := strings.Index(s[i+len(l.bOpen):], l.bClose)
			if j < 0 {
				h.block = true
				return tok(dst, hlComment, s[i:])
			}
			e := i + len(l.bOpen) + j + len(l.bClose)
			dst = tok(dst, hlComment, s[i:e])
			i, plain = e, e
			continue
		}
		for _, p := range l.line {
			if c == p[0] && strings.HasPrefix(s[i:], p) && (!l.wordComment || i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
				dst = append(dst, s[plain:i]...)
				return tok(dst, hlComment, s[i:])
			}
		}
		if strings.IndexByte(l.quotes, c) >= 0 {
			if c == '\'' && l.charQ {
				e := charLitEnd(s, i)
				if e < 0 {
					i++
					continue
				}
				dst = append(dst, s[plain:i]...)
				dst = tok(dst, hlString, s[i:e])
				i, plain = e, e
				continue
			}
			triple := l.triple && i+2 < n && s[i+1] == c && s[i+2] == c
			open := 1
			if triple {
				open = 3
			}
			end, closed := h.scanStr(s, i+open, c, triple)
			color := hlString
			if l.jsonKeys && closed && nextNonSpace(s, end) == ':' {
				color = hlKey
			}
			dst = append(dst, s[plain:i]...)
			dst = tok(dst, color, s[i:end])
			if !closed {
				if triple || strings.IndexByte(l.multi, c) >= 0 {
					h.str, h.triple = c, triple
				}
				return dst
			}
			i, plain = end, end
			continue
		}
		if isDigit(c) && (i == 0 || !isIdent(s[i-1])) {
			j := i + 1
			for j < n && (isIdent(s[j]) || s[j] == '.' && j+1 < n && isDigit(s[j+1])) {
				j++
			}
			dst = append(dst, s[plain:i]...)
			dst = tok(dst, hlNumber, s[i:j])
			i, plain = j, j
			continue
		}
		if l.vars && c == '$' && i+1 < n {
			j := i + 1
			if s[j] == '{' {
				if k := strings.IndexByte(s[j:], '}'); k >= 0 {
					j += k + 1
				}
			} else {
				for j < n && isIdent(s[j]) {
					j++
				}
				if j == i+1 && strings.IndexByte("@*#?$!-", s[j]) >= 0 {
					j++
				}
			}
			if j > i+1 {
				dst = append(dst, s[plain:i]...)
				dst = tok(dst, hlKey, s[i:j])
				i, plain = j, j
				continue
			}
		}
		if isIdentStart(c) || l.directive && c == '#' && strings.TrimLeft(s[:i], " \t") == "" {
			j := i + 1
			for j < n && isIdent(s[j]) {
				j++
			}
			if i > 0 && (isIdent(s[i-1]) || s[i-1] == '$') {
				i = j
				continue
			}
			color := ""
			switch h.kind(s[i:j]) {
			case kwKeyword:
				color = hlKeyword
			case kwConst:
				color = hlNumber
			case kwType:
				color = hlType
			default:
				if s[i] == '#' {
					color = hlKeyword
				} else if nextNonSpace(s, j) == '(' {
					color = hlFunc
				}
			}
			if color != "" {
				dst = append(dst, s[plain:i]...)
				dst = tok(dst, color, s[i:j])
				plain = j
			}
			i = j
			continue
		}
		i++
	}
	return append(dst, s[plain:]...)
}

func (h *hlState) kind(w string) byte {
	if !h.lang.fold || len(w) > 32 {
		return h.lang.kw[w]
	}
	var b [32]byte
	for i := range len(w) {
		c := w[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return h.lang.kw[string(b[:len(w)])]
}

// key highlights a yaml/toml key at the start of s.
func (h *hlState) key(dst []byte, s string) ([]byte, int) {
	p := 0
	for p < len(s) && (s[p] == ' ' || s[p] == '\t') {
		p++
	}
	if h.lang.keys == ':' && strings.HasPrefix(s[p:], "- ") {
		p += 2
	}
	if h.lang.keys == '=' && p < len(s) && s[p] == '[' {
		return tok(append(dst, s[:p]...), hlKey, s[p:]), len(s)
	}
	j := strings.IndexByte(s[p:], h.lang.keys)
	if j <= 0 {
		return dst, 0
	}
	j += p
	k := s[p:j]
	if strings.ContainsAny(k, "#\"'{[") || h.lang.keys == ':' && j+1 < len(s) && s[j+1] != ' ' {
		return dst, 0
	}
	dst = append(dst, s[:p]...)
	return tok(dst, hlKey, k), j
}

// charLitEnd returns the end of a char literal like 'x' or '\n' at s[i], or -1.
func charLitEnd(s string, i int) int {
	if i+2 >= len(s) {
		return -1
	}
	if s[i+1] == '\\' {
		if e := strings.IndexByte(s[i+2:min(len(s), i+12)], '\''); e > 0 {
			return i + 2 + e + 1
		}
		return -1
	}
	j := i + 1
	if s[j] >= 0x80 {
		for j+1 < len(s) && s[j+1] >= 0x80 && s[j+1] < 0xc0 {
			j++
		}
	}
	if j+1 < len(s) && s[j+1] == '\'' {
		return j + 2
	}
	return -1
}
