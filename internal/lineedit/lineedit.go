// Package lineedit is the interactive terminal UI: an inline (normal screen,
// native scrollback) footer pinned below streaming content, holding an
// always-active multi-line editor with history, bracketed paste and a
// slash-command autocomplete popup, plus status lines and a modal picker. No
// TUI framework.
package lineedit

type action int

const (
	actNone action = iota
	actRedraw
	actClearScreen
	actSubmit
	actInterrupt
	actEOF
)

// session is the terminal-independent editing state of the input editor.
type session struct {
	buf     buffer
	hist    *history
	histIdx int // len(hist.entries) = editing the draft
	draft   string
	sug     suggestState
}

// reset clears the buffer after a submit and leaves history browsing.
func (s *session) reset() {
	s.set("")
}

// set replaces the buffer (cursor at end) and leaves history browsing.
func (s *session) set(text string) {
	s.setAt(text, len([]rune(text)))
}

// setAt replaces the buffer with the cursor at pos (runes).
func (s *session) setAt(text string, pos int) {
	s.buf.set(text)
	s.buf.pos = min(max(pos, 0), len(s.buf.text))
	s.draft = ""
	s.histIdx = len(s.hist.entries)
	s.sug.sync(s.buf.text, s.buf.pos)
}

// handle applies k to the session and reports what the caller must do.
func (s *session) handle(k key) action {
	if s.sug.visible() {
		if a, ok := s.handleSuggest(k); ok {
			return a
		}
	}
	a := s.edit(k)
	s.sug.sync(s.buf.text, s.buf.pos)
	return a
}

// handleSuggest applies the keys the popup owns. ok is false for keys that
// edit the buffer normally.
func (s *session) handleSuggest(k key) (action, bool) {
	switch k.kind {
	case keyUp:
		s.sug.move(-1)
	case keyDown:
		s.sug.move(1)
	case keyEscape:
		s.sug.dismiss(s.buf.text, s.buf.pos)
	case keyTab:
		text, cur, _ := s.sug.accepted(true)
		s.setAt(text, cur)
	case keyEnter:
		text, cur, final := s.sug.accepted(false)
		s.setAt(text, cur)
		if final {
			return actSubmit, true
		}
	default:
		return actNone, false
	}
	return actRedraw, true
}

// edit applies an editing key to the buffer.
func (s *session) edit(k key) action {
	b := &s.buf
	switch k.kind {
	case keyNone, keyCtrlV, keyEscape:
		return actNone
	case keyRune:
		b.insert(k.r)
	case keyPaste:
		if k.text == "" {
			return actNone
		}
		b.insert([]rune(k.text)...)
	case keyNewline:
		b.insert('\n')
	case keyEnter:
		if b.pos > 0 && b.text[b.pos-1] == '\\' {
			b.backspace()
			b.insert('\n')
			return actRedraw
		}
		return actSubmit
	case keyBackspace:
		b.backspace()
	case keyDelete:
		b.deleteForward()
	case keyLeft:
		b.left()
	case keyRight:
		b.right()
	case keyWordLeft:
		b.wordLeft()
	case keyWordRight:
		b.wordRight()
	case keyHome:
		b.home()
	case keyEnd:
		b.end()
	case keyKillLineStart:
		b.killLineStart()
	case keyKillLineEnd:
		b.killLineEnd()
	case keyKillWordBack:
		b.killWordBack()
	case keyKillWordForward:
		b.killWordForward()
	case keyUp:
		if !b.up() {
			s.historyMove(-1)
		}
	case keyDown:
		if !b.down() {
			s.historyMove(1)
		}
	case keyClearScreen:
		return actClearScreen
	case keyInterrupt:
		if b.empty() {
			return actInterrupt
		}
		b.set("")
		s.histIdx = len(s.hist.entries)
	case keyEOF:
		if b.empty() {
			return actEOF
		}
		b.deleteForward()
	case keyTab:
		// Tab only accepts suggestions; keep it out of slash commands.
		if len(b.text) > 0 && b.text[0] == '/' {
			return actNone
		}
		b.insert('\t')
	}
	return actRedraw
}

// historyMove steps through history (-1 older, +1 newer), saving the draft
// when leaving it and restoring it when coming back.
func (s *session) historyMove(dir int) {
	entries := s.hist.entries
	s.histIdx = min(s.histIdx, len(entries))
	next := s.histIdx + dir
	if next < 0 || next > len(entries) {
		return
	}
	if s.histIdx == len(entries) {
		s.draft = s.buf.String()
	}
	s.histIdx = next
	if next == len(entries) {
		s.buf.set(s.draft)
	} else {
		s.buf.set(entries[next])
	}
	// A recalled entry does not pop up suggestions, so Up/Down keep
	// browsing history; editing it does.
	s.sug.dismiss(s.buf.text, s.buf.pos)
}
