package markdown

import (
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// Stream renders markdown arriving in arbitrary deltas.
//
// The current incomplete line is echoed raw as it arrives. When its newline
// arrives the echo is erased and the styled line printed. Tables and mermaid
// blocks stay raw until complete and are then replaced as a whole, but only
// when the raw rows still fit on screen (which needs w to be a terminal
// *os.File); otherwise the styled version is printed below them.
//
// If w is an *os.File that is not a terminal, nothing is echoed or erased:
// only finished lines are written. Other writers are treated as a terminal of
// unknown height.
type Stream struct {
	w      io.Writer
	o      Options
	m      machine
	live   bool
	height func() int

	line  []byte // current incomplete line, without '\r'
	shown int    // bytes of line echoed raw on screen
	rows  int    // terminal rows occupied by held lines
	out   []byte
}

// NewStream returns a Stream writing to w.
func NewStream(w io.Writer, o Options) *Stream {
	s := &Stream{w: w, o: o, live: true}
	if o.Height != nil {
		s.height = o.Height
	} else if f, ok := w.(*os.File); ok {
		fd := int(f.Fd())
		if term.IsTerminal(fd) {
			s.height = func() int {
				_, h, err := term.GetSize(fd)
				if err != nil {
					return 0
				}
				return h
			}
		} else {
			s.live = false
		}
	}
	s.m = machine{out: s, color: o.Color, width: s.width}
	return s
}

func (s *Stream) width() int {
	if s.o.Width != nil {
		if w := s.o.Width(); w > 0 {
			return w
		}
	}
	return 80
}

// Write renders the next piece of the message.
func (s *Stream) Write(delta string) {
	for len(delta) > 0 {
		i := strings.IndexByte(delta, '\n')
		chunk := delta
		if i >= 0 {
			chunk, delta = delta[:i], delta[i+1:]
		}
		for len(chunk) > 0 {
			j := strings.IndexByte(chunk, '\r')
			if j < 0 {
				s.line = append(s.line, chunk...)
				break
			}
			s.line = append(s.line, chunk[:j]...)
			chunk = chunk[j+1:]
		}
		if i < 0 {
			if s.live && len(s.line) > s.shown {
				s.w.Write(s.line[s.shown:])
				s.shown = len(s.line)
			}
			return
		}
		s.m.line(string(s.line))
		s.line, s.shown = s.line[:0], 0
	}
}

// Close ends the message: it renders a final line without newline and any
// pending table or diagram, and resets the state so s can be reused.
func (s *Stream) Close() {
	if len(s.line) > 0 {
		s.m.line(string(s.line))
	}
	s.m.end()
	s.line, s.shown, s.rows = s.line[:0], 0, 0
}

// emit implements sink: erase the held rows (if possible) and the echo of the
// current line, then print b.
func (s *Stream) emit(b []byte) {
	if !s.live {
		s.w.Write(b)
		return
	}
	out := s.out[:0]
	up, erase := 0, false
	if s.shown > 0 {
		erase = true
		if pw := visWidth(s.line[:s.shown]); pw > 0 {
			up = (pw - 1) / s.width()
		}
	}
	h := 0
	if s.height != nil && (s.rows > 0 || up > 0) {
		h = s.height()
	}
	if erase && h > 0 && up+1 >= h-2 {
		erase, up = false, 0
		out = append(out, '\n')
	}
	if s.rows > 0 && h > 0 && s.rows+up+1 < h-2 {
		up += s.rows
		erase = true
	}
	if erase {
		out = append(out, '\r')
		if up > 0 {
			out = append(out, "\x1b["...)
			out = strconv.AppendInt(out, int64(up), 10)
			out = append(out, 'A')
		}
		out = append(out, "\x1b[J"...)
	}
	out = append(out, b...)
	s.w.Write(out)
	s.out = out[:0]
	s.rows, s.shown = 0, 0
}

// hold implements sink: finish the raw echo of the current line.
func (s *Stream) hold() {
	if !s.live {
		return
	}
	out := append(s.out[:0], s.line[s.shown:]...)
	out = append(out, '\n')
	s.w.Write(out)
	s.out = out[:0]
	s.shown = 0
	w := s.width()
	s.rows += max(1, (visWidth(s.line)+w-1)/w)
}
