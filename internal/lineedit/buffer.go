package lineedit

import (
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
)

// widthCond is a fixed, locale-independent width table so layout does not
// depend on RUNEWIDTH_EASTASIAN or LANG.
var widthCond = &runewidth.Condition{}

// tabWidth is the number of cells a tab occupies when rendered.
const tabWidth = 4

// runeCells returns the number of terminal cells r occupies when rendered.
func runeCells(r rune) int {
	if r == '\t' {
		return tabWidth
	}
	return widthCond.RuneWidth(r)
}

// buffer is a multi-line edit buffer: lines are separated by '\n' and pos is
// the cursor index into text (0..len(text)).
type buffer struct {
	text []rune
	pos  int
}

func (b *buffer) String() string { return string(b.text) }

func (b *buffer) empty() bool { return len(b.text) == 0 }

// set replaces the content and moves the cursor to the end.
func (b *buffer) set(s string) {
	b.text = []rune(s)
	b.pos = len(b.text)
}

func (b *buffer) insert(rs ...rune) {
	if len(rs) == 0 {
		return
	}
	b.text = append(b.text, rs...)
	copy(b.text[b.pos+len(rs):], b.text[b.pos:])
	copy(b.text[b.pos:], rs)
	b.pos += len(rs)
}

// deleteRange removes text[from:to] and leaves the cursor at from.
func (b *buffer) deleteRange(from, to int) {
	if from >= to {
		return
	}
	b.text = append(b.text[:from], b.text[to:]...)
	b.pos = from
}

func (b *buffer) backspace() {
	if b.pos > 0 {
		b.deleteRange(b.pos-1, b.pos)
	}
}

func (b *buffer) deleteForward() {
	if b.pos < len(b.text) {
		b.deleteRange(b.pos, b.pos+1)
	}
}

func (b *buffer) left() {
	if b.pos > 0 {
		b.pos--
	}
}

func (b *buffer) right() {
	if b.pos < len(b.text) {
		b.pos++
	}
}

// lineStart returns the index of the first rune of the line containing i.
func (b *buffer) lineStart(i int) int {
	for i > 0 && b.text[i-1] != '\n' {
		i--
	}
	return i
}

// lineEnd returns the index of the '\n' ending the line containing i, or len(text).
func (b *buffer) lineEnd(i int) int {
	for i < len(b.text) && b.text[i] != '\n' {
		i++
	}
	return i
}

func (b *buffer) home() { b.pos = b.lineStart(b.pos) }
func (b *buffer) end()  { b.pos = b.lineEnd(b.pos) }

func (b *buffer) onFirstLine() bool { return b.lineStart(b.pos) == 0 }
func (b *buffer) onLastLine() bool  { return b.lineEnd(b.pos) == len(b.text) }

// column returns the display column of i within its logical line.
func (b *buffer) column(i int) int {
	w := 0
	for _, r := range b.text[b.lineStart(i):i] {
		w += runeCells(r)
	}
	return w
}

// seekColumn returns the index in the line starting at start that is closest
// to display column col without passing it.
func (b *buffer) seekColumn(start, col int) int {
	end := b.lineEnd(start)
	w := 0
	i := start
	for i < end {
		cw := runeCells(b.text[i])
		if w+cw > col {
			break
		}
		w += cw
		i++
	}
	return i
}

// up moves to the previous logical line keeping the display column. It
// reports false when the cursor is already on the first line.
func (b *buffer) up() bool {
	ls := b.lineStart(b.pos)
	if ls == 0 {
		return false
	}
	col := b.column(b.pos)
	b.pos = b.seekColumn(b.lineStart(ls-1), col)
	return true
}

// down moves to the next logical line keeping the display column. It
// reports false when the cursor is already on the last line.
func (b *buffer) down() bool {
	le := b.lineEnd(b.pos)
	if le == len(b.text) {
		return false
	}
	col := b.column(b.pos)
	b.pos = b.seekColumn(le+1, col)
	return true
}

type runeClass int

const (
	classSpace runeClass = iota
	classWord
	classPunct
)

func classOf(r rune) runeClass {
	switch {
	case unicode.IsSpace(r):
		return classSpace
	case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r):
		return classWord
	default:
		return classPunct
	}
}

// wordStartBefore returns the start of the word before i: whitespace is
// skipped, then a run of runes of the same class (word or punctuation).
func (b *buffer) wordStartBefore(i int) int {
	for i > 0 && classOf(b.text[i-1]) == classSpace {
		i--
	}
	if i == 0 {
		return 0
	}
	c := classOf(b.text[i-1])
	for i > 0 && classOf(b.text[i-1]) == c {
		i--
	}
	return i
}

// wordEndAfter returns the end of the word after i (mirror of wordStartBefore).
func (b *buffer) wordEndAfter(i int) int {
	n := len(b.text)
	for i < n && classOf(b.text[i]) == classSpace {
		i++
	}
	if i == n {
		return n
	}
	c := classOf(b.text[i])
	for i < n && classOf(b.text[i]) == c {
		i++
	}
	return i
}

func (b *buffer) wordLeft()  { b.pos = b.wordStartBefore(b.pos) }
func (b *buffer) wordRight() { b.pos = b.wordEndAfter(b.pos) }

func (b *buffer) killWordBack()    { b.deleteRange(b.wordStartBefore(b.pos), b.pos) }
func (b *buffer) killWordForward() { b.deleteRange(b.pos, b.wordEndAfter(b.pos)) }

// killLineStart deletes from the start of the current line to the cursor.
// At the start of a line it joins with the previous line instead.
func (b *buffer) killLineStart() {
	ls := b.lineStart(b.pos)
	if ls == b.pos {
		b.backspace()
		return
	}
	b.deleteRange(ls, b.pos)
}

// killLineEnd deletes from the cursor to the end of the current line. At the
// end of a line it joins with the next line instead.
func (b *buffer) killLineEnd() {
	le := b.lineEnd(b.pos)
	if le == b.pos {
		b.deleteForward()
		return
	}
	b.deleteRange(b.pos, le)
}

// normalizePaste converts pasted bytes to buffer text: CRLF and lone CR become
// LF, ESC and other control characters except LF and TAB are dropped, and
// invalid UTF-8 is removed.
func normalizePaste(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ToValidUTF8(s, "")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}
