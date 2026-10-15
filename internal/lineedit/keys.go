package lineedit

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

type keyKind int

const (
	keyNone keyKind = iota // unknown or ignored input
	keyRune
	keyEnter
	keyNewline // insert a literal newline (Alt+Enter, Ctrl+J, Shift+Enter)
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyWordLeft
	keyWordRight
	keyKillLineStart
	keyKillLineEnd
	keyKillWordBack
	keyKillWordForward
	keyClearScreen
	keyInterrupt // Ctrl+C
	keyEOF       // Ctrl+D
	keyTab
	keyPaste
	keyCtrlV
	keyEscape   // Escape: a lone ESC (synthesized by the input loop) or the kitty protocol's CSI 27 u
	keyAltEnter // Alt+Enter: a newline, or while busy a follow-up (see TUI.SetBusy)
	keyAltUp    // Alt+Up: restore queued messages (EvDequeue)
	keyCtrlR    // Ctrl+R (pickers: rename)
	keyCtrlS    // Ctrl+S (pickers: sort)
	keyBackTab  // Shift+Tab
)

type key struct {
	kind keyKind
	r    rune   // keyRune
	text string // keyPaste, already normalized
}

const (
	pasteEnd = "\x1b[201~"
	// maxSeqLen bounds CSI parameter bytes so a malformed sequence cannot
	// swallow unbounded input.
	maxSeqLen = 64
)

// keyParser decodes terminal input bytes into keys.
type keyParser struct {
	r *bufio.Reader
}

// next returns the next key. Unknown escape sequences yield keyNone.
func (p *keyParser) next() (key, error) {
	c, err := p.r.ReadByte()
	if err != nil {
		return key{}, err
	}
	switch {
	case c == 0x1b:
		return p.escape()
	case c < 0x20 || c == 0x7f:
		return controlKey(c), nil
	case c < utf8.RuneSelf:
		return key{kind: keyRune, r: rune(c)}, nil
	}
	return p.utf8Rune(c)
}

// controlKey maps a control byte (Ctrl+letter, Backspace) to its key; the
// kitty protocol and modifyOtherKeys report the same keys as Ctrl+code.
func controlKey(c byte) key {
	switch c {
	case 0x01:
		return key{kind: keyHome}
	case 0x02:
		return key{kind: keyLeft}
	case 0x03:
		return key{kind: keyInterrupt}
	case 0x04:
		return key{kind: keyEOF}
	case 0x05:
		return key{kind: keyEnd}
	case 0x06:
		return key{kind: keyRight}
	case 0x08, 0x7f:
		return key{kind: keyBackspace}
	case '\t':
		return key{kind: keyTab}
	case '\n':
		return key{kind: keyNewline}
	case 0x0b:
		return key{kind: keyKillLineEnd}
	case 0x0c:
		return key{kind: keyClearScreen}
	case '\r':
		return key{kind: keyEnter}
	case 0x0e:
		return key{kind: keyDown}
	case 0x10:
		return key{kind: keyUp}
	case 0x15:
		return key{kind: keyKillLineStart}
	case 0x17:
		return key{kind: keyKillWordBack}
	case 0x16:
		return key{kind: keyCtrlV}
	case 0x12:
		return key{kind: keyCtrlR}
	case 0x13:
		return key{kind: keyCtrlS}
	}
	return key{}
}

// utf8Rune completes a multi-byte UTF-8 sequence starting with lead byte c.
// Invalid sequences are ignored.
func (p *keyParser) utf8Rune(c byte) (key, error) {
	var n int
	switch {
	case c&0xe0 == 0xc0:
		n = 2
	case c&0xf0 == 0xe0:
		n = 3
	case c&0xf8 == 0xf0:
		n = 4
	default:
		return key{}, nil
	}
	buf := [4]byte{c}
	for i := 1; i < n; i++ {
		b, err := p.r.ReadByte()
		if err != nil {
			return key{}, err
		}
		if b&0xc0 != 0x80 {
			// Not a continuation byte: drop the broken prefix, reprocess b.
			_ = p.r.UnreadByte()
			return key{}, nil
		}
		buf[i] = b
	}
	r, _ := utf8.DecodeRune(buf[:n])
	if r == utf8.RuneError {
		return key{}, nil
	}
	return key{kind: keyRune, r: r}, nil
}

// escape parses what follows an ESC byte. A bare ESC acts as a Meta prefix
// for the next key, matching readline.
func (p *keyParser) escape() (key, error) {
	c, err := p.r.ReadByte()
	if err != nil {
		return key{}, err
	}
	switch c {
	case '[':
		return p.csi()
	case 'O':
		return p.ss3()
	case 0x1b:
		// ESC ESC [ X: some terminals (macOS Terminal, iTerm2) send Alt+Arrow this way.
		k, err := p.escape()
		if err != nil {
			return k, err
		}
		switch k.kind {
		case keyLeft:
			k.kind = keyWordLeft
		case keyRight:
			k.kind = keyWordRight
		case keyUp:
			k.kind = keyAltUp
		}
		return k, nil
	}
	return altKey(c), nil
}

// altKey maps Alt (Meta) plus a key: "ESC c" in legacy encoding, Alt+code
// from the kitty protocol and modifyOtherKeys.
func altKey(c byte) key {
	switch c {
	case '\r':
		return key{kind: keyAltEnter}
	case '\n':
		return key{kind: keyNewline}
	case 0x7f, 0x08:
		return key{kind: keyKillWordBack}
	case 'b', 'B':
		return key{kind: keyWordLeft}
	case 'f', 'F':
		return key{kind: keyWordRight}
	case 'd', 'D':
		return key{kind: keyKillWordForward}
	}
	return key{}
}

func (p *keyParser) ss3() (key, error) {
	c, err := p.r.ReadByte()
	if err != nil {
		return key{}, err
	}
	switch c {
	case 'A':
		return key{kind: keyUp}, nil
	case 'B':
		return key{kind: keyDown}, nil
	case 'C':
		return key{kind: keyRight}, nil
	case 'D':
		return key{kind: keyLeft}, nil
	case 'H':
		return key{kind: keyHome}, nil
	case 'F':
		return key{kind: keyEnd}, nil
	case 'M':
		return key{kind: keyEnter}, nil
	}
	return key{}, nil
}

// csi parses a CSI sequence after "ESC [": parameter bytes 0x30–0x3F,
// intermediate bytes 0x20–0x2F, then a final byte 0x40–0x7E.
func (p *keyParser) csi() (key, error) {
	var params []byte
	for {
		c, err := p.r.ReadByte()
		if err != nil {
			return key{}, err
		}
		if c >= 0x40 && c <= 0x7e {
			return p.csiKey(string(params), c)
		}
		if c < 0x20 || c > 0x3f {
			// Not a valid CSI byte; abandon the sequence and reprocess c.
			_ = p.r.UnreadByte()
			return key{}, nil
		}
		if len(params) < maxSeqLen {
			params = append(params, c)
		}
	}
}

// csiParams splits "a;b;c" into integers; empty or non-numeric fields are 0.
// Sub-parameters (kitty "a:b") keep only the first value.
func csiParams(s string) []int {
	if s == "" {
		return nil
	}
	fields := strings.Split(s, ";")
	out := make([]int, len(fields))
	for i, f := range fields {
		if j := strings.IndexByte(f, ':'); j >= 0 {
			f = f[:j]
		}
		out[i], _ = strconv.Atoi(f)
	}
	return out
}

// Modifier bits (xterm encoding is 1 + bits).
const (
	modShift = 1
	modAlt   = 2
	modCtrl  = 4
	modMeta  = 8
)

// modBits reads the modifier parameter; the kitty protocol's Caps Lock and
// Num Lock bits are dropped: they don't change what a key does here.
func modBits(params []int, idx int) int {
	if idx < len(params) && params[idx] > 1 {
		return (params[idx] - 1) &^ (64 | 128)
	}
	return 0
}

func (p *keyParser) csiKey(params string, final byte) (key, error) {
	if params != "" && (params[0] == '<' || params[0] == '>' || params[0] == '?' || params[0] == '=') {
		return key{}, nil // mouse reports, DA responses, etc.
	}
	ps := csiParams(params)
	mods := modBits(ps, 1)
	wordMod := mods&(modAlt|modCtrl|modMeta) != 0
	switch final {
	case 'A':
		if mods&(modAlt|modMeta) != 0 {
			return key{kind: keyAltUp}, nil
		}
		return key{kind: keyUp}, nil
	case 'B':
		return key{kind: keyDown}, nil
	case 'C':
		if wordMod {
			return key{kind: keyWordRight}, nil
		}
		return key{kind: keyRight}, nil
	case 'D':
		if wordMod {
			return key{kind: keyWordLeft}, nil
		}
		return key{kind: keyLeft}, nil
	case 'H':
		return key{kind: keyHome}, nil
	case 'F':
		return key{kind: keyEnd}, nil
	case 'Z':
		return key{kind: keyBackTab}, nil
	case '~':
		if len(ps) == 0 {
			return key{}, nil
		}
		switch ps[0] {
		case 1, 7:
			return key{kind: keyHome}, nil
		case 4, 8:
			return key{kind: keyEnd}, nil
		case 3:
			if wordMod {
				return key{kind: keyKillWordForward}, nil
			}
			return key{kind: keyDelete}, nil
		case 200:
			return p.paste()
		case 27:
			// xterm modifyOtherKeys: CSI 27 ; mod ; code ~
			if len(ps) >= 3 {
				return modifiedKey(ps[2], modBits(ps, 1)), nil
			}
		}
		return key{}, nil
	case 'u':
		// kitty keyboard protocol: CSI code ; mod u
		if len(ps) >= 1 {
			return modifiedKey(ps[0], mods), nil
		}
		return key{}, nil
	}
	return key{}, nil
}

// modifiedKey maps a key code with modifiers (from CSI u / modifyOtherKeys)
// to the key the legacy encoding gives, so both behave the same.
func modifiedKey(code, mods int) key {
	alt := mods&(modAlt|modMeta) != 0
	ctrl := mods&modCtrl != 0
	switch code {
	case 13:
		switch {
		case mods == 0:
			return key{kind: keyEnter}
		case mods&(modAlt|modMeta) != 0 && mods&(modShift|modCtrl) == 0:
			return key{kind: keyAltEnter}
		}
		return key{kind: keyNewline}
	case 9:
		if mods&modShift != 0 {
			return key{kind: keyBackTab} // kitty reports Shift+Tab as CSI 9;2u
		}
		return key{kind: keyTab}
	case 127, 8:
		if mods&(modAlt|modCtrl|modMeta) != 0 {
			return key{kind: keyKillWordBack}
		}
		return key{kind: keyBackspace}
	}
	switch {
	case code == 27:
		if mods == 0 {
			return key{kind: keyEscape} // the kitty protocol reports Escape as CSI 27 u
		}
		return key{}
	case ctrl && !alt:
		if code >= 'A' && code <= 'Z' {
			code += 'a' - 'A'
		}
		if (code >= 'a' && code <= 'z') || strings.ContainsRune("@[\\]^_", rune(code)) {
			return controlKey(byte(code) & 0x1f)
		}
		return key{}
	case alt && !ctrl:
		if code < utf8.RuneSelf {
			return altKey(byte(code))
		}
		return key{}
	case ctrl:
		return key{}
	}
	if code < 0x20 || code == 0x7f || !utf8.ValidRune(rune(code)) {
		return key{}
	}
	if mods&modShift != 0 && code >= 'a' && code <= 'z' {
		code -= 'a' - 'A' // reported as the base key plus Shift
	}
	return key{kind: keyRune, r: rune(code)}
}

// paste reads bracketed-paste content up to the end marker.
func (p *keyParser) paste() (key, error) {
	var buf bytes.Buffer
	end := []byte(pasteEnd)
	for {
		c, err := p.r.ReadByte()
		if err != nil {
			return key{kind: keyPaste, text: normalizePaste(buf.String())}, err
		}
		buf.WriteByte(c)
		if c == '~' && bytes.HasSuffix(buf.Bytes(), end) {
			buf.Truncate(buf.Len() - len(end))
			return key{kind: keyPaste, text: normalizePaste(buf.String())}, nil
		}
	}
}
