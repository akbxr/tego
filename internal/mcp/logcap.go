package mcp

import (
	"bytes"
	"fmt"
	"os"
	"sync"
)

// logLimit is how large a server's log grows before it moves to
// <log>.1, replacing the one there: the two hold at most twice this.
const logLimit = 1 << 20

// serverLog is a server's log (stderr and stray stdout). It writes whole
// lines, folds a line repeated over and over (a server that says "waiting
// for the plugin..." each second) into a count, and moves the file aside
// when it outgrows logLimit, so it never grows without bound.
type serverLog struct {
	mu       sync.Mutex
	path     string
	limit    int64
	f        *os.File
	size     int64
	start    int64  // where this run's output starts in the file
	partial  []byte // a line not ended yet
	last     []byte // the last line written, when haveLast
	haveLast bool
	repeats  int // times last came again, not written yet
	buf      []byte
}

func openServerLog(path string, limit int64) (*serverLog, error) {
	l := &serverLog{path: path, limit: limit}
	if err := l.open(false); err != nil {
		return nil, err
	}
	if l.size >= limit { // grown by an older tego: start afresh
		if l.size > limit { // its end is enough for .1
			keepEnd(path, limit)
		}
		l.rotate()
	}
	l.start = l.size
	return l, nil
}

func (l *serverLog) open(truncate bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	if truncate {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(l.path, flags, 0o600)
	if err != nil {
		return err
	}
	l.f, l.size = f, 0
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
	return nil
}

// keepEnd cuts the file at path to its last limit bytes, from a line start.
func keepEnd(path string, limit int64) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return
	}
	buf := make([]byte, limit)
	n, _ := f.ReadAt(buf, st.Size()-limit)
	f.Close()
	buf = buf[:n]
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	tmp := path + ".cut"
	if os.WriteFile(tmp, buf, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// rotate moves the log to <log>.1 and starts an empty one.
func (l *serverLog) rotate() {
	l.f.Close()
	_ = os.Rename(l.path, l.path+".1")
	if l.open(true) != nil {
		l.f = nil
		return
	}
	l.start = 0
	l.put([]byte("--- earlier output: " + l.path + ".1\n"))
}

func (l *serverLog) put(b []byte) {
	if l.f == nil {
		return
	}
	n, _ := l.f.Write(b)
	l.size += int64(n)
}

// Write takes output in any pieces; lines reach the file whole.
func (l *serverLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			l.partial = append(l.partial, p...)
			if len(l.partial) > 64<<10 { // a "line" without end: write it as it is
				l.line(l.partial)
				l.partial = l.partial[:0]
			}
			break
		}
		line := p[:i]
		if len(l.partial) > 0 {
			line = append(l.partial, line...)
		}
		l.line(line)
		l.partial = l.partial[:0]
		p = p[i+1:]
	}
	return n, nil
}

func (l *serverLog) line(line []byte) {
	if l.haveLast && bytes.Equal(line, l.last) {
		l.repeats++
		return
	}
	l.flushRepeats()
	l.buf = append(append(l.buf[:0], line...), '\n') // not into the caller's p
	l.put(l.buf)
	l.last, l.haveLast = append(l.last[:0], line...), true
	if l.size >= l.limit {
		l.rotate()
		l.haveLast = false // the new file shows the line once more if it comes again
	}
}

func (l *serverLog) flushRepeats() {
	if l.repeats > 0 {
		word := "times"
		if l.repeats == 1 {
			word = "time"
		}
		l.put(fmt.Appendf(nil, "(the line above came %d more %s)\n", l.repeats, word))
		l.repeats = 0
	}
}

// runStart is where this run's output starts in the file now.
func (l *serverLog) runStart() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.start
}

// sync writes what is held back (a line without end, a repeat count).
func (l *serverLog) sync() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) > 0 {
		l.line(l.partial)
		l.partial = l.partial[:0]
	}
	l.flushRepeats()
}

func (l *serverLog) Close() error {
	l.sync()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
