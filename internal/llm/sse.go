package llm

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// sseEvent is one dispatched server-sent event. Data is only valid until the
// next call to sseReader.Next.
type sseEvent struct {
	Event string
	Data  []byte
}

// sseReader is a minimal text/event-stream parser: it understands `event:`
// and `data:` fields (multi-line data joined with '\n'), ignores comments
// and other fields, and dispatches on blank lines.
type sseReader struct {
	r     *bufio.Reader
	line  []byte
	data  []byte
	event []byte
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// readLine returns the next line without its terminator. The slice is
// reused between calls.
func (s *sseReader) readLine() ([]byte, error) {
	s.line = s.line[:0]
	for {
		chunk, err := s.r.ReadSlice('\n')
		s.line = append(s.line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(s.line) > 0 {
				return trimEOL(s.line), nil
			}
			return nil, err
		}
		return trimEOL(s.line), nil
	}
}

func trimEOL(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte{'\n'})
	return bytes.TrimSuffix(b, []byte{'\r'})
}

// Next returns the next event that carries data. It returns io.EOF when the
// stream ends cleanly.
func (s *sseReader) Next() (sseEvent, error) {
	s.data = s.data[:0]
	s.event = s.event[:0]
	hasData := false
	for {
		line, err := s.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && hasData {
				return sseEvent{Event: string(s.event), Data: s.data}, nil
			}
			return sseEvent{}, err
		}
		if len(line) == 0 {
			if hasData {
				return sseEvent{Event: string(s.event), Data: s.data}, nil
			}
			s.event = s.event[:0]
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}
		switch string(field) {
		case "data":
			if hasData {
				s.data = append(s.data, '\n')
			}
			s.data = append(s.data, value...)
			hasData = true
		case "event":
			s.event = append(s.event[:0], value...)
		}
	}
}
