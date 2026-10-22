package mcp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// stdioTransport runs the server as a child process speaking
// newline-delimited JSON on stdin/stdout.
type stdioTransport struct {
	cmd     *exec.Cmd
	stdin   *os.File
	stdout  *os.File
	wmu     sync.Mutex
	exited  chan struct{}
	waitErr error
	closing atomic.Bool
	once    sync.Once

	log     *serverLog // nil when logDir is empty
	logPath string
	stderr  *os.File // the server's stderr, read into log
	stderrW *os.File // its other end, ours until the server starts
	// copied is closed when all the server's stderr is in the log.
	copied chan struct{}
}

func startStdio(c *conn, cfg ServerConfig, cwd, logDir string) (*stdioTransport, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("no command configured")
	}
	t := &stdioTransport{exited: make(chan struct{})}
	cmd := exec.Command(expandHome(cfg.Command), cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Dir = cwd
	if cfg.Cwd != "" {
		dir := expandHome(cfg.Cwd)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		cmd.Dir = dir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o700); err == nil {
			path := filepath.Join(logDir, "mcp-"+sanitize(cfg.Name)+".log")
			if l, err := openServerLog(path, logLimit); err == nil {
				if r, w, err := os.Pipe(); err == nil {
					t.log, t.logPath, t.stderr, t.stderrW = l, path, r, w
					fmt.Fprintf(l, "--- %s start: %s %s\n", time.Now().Format(time.RFC3339), cfg.Command, strings.Join(cfg.Args, " "))
					// Our own pipe, read by our goroutine: Wait doesn't wait
					// on it, as with stdout.
					cmd.Stderr = w
				} else {
					l.Close()
				}
			}
		}
	}

	inR, inW, err := os.Pipe()
	if err != nil {
		t.closeLog()
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		t.closeLog()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout = inR, outW
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		t.closeLog()
		return nil, err
	}
	inR.Close()
	outW.Close()
	if t.stderr != nil {
		t.stderrW.Close()
		t.stderrW = nil
		t.copied = make(chan struct{})
		go func() {
			_, _ = io.Copy(t.log, t.stderr)
			close(t.copied)
		}()
	}
	t.cmd, t.stdin, t.stdout = cmd, inW, outR
	c.t = t

	go func() {
		t.waitErr = cmd.Wait()
		close(t.exited)
		// A grandchild may keep stdout open after the server exits; don't
		// let that keep the reader alive.
		time.AfterFunc(500*time.Millisecond, func() { t.stdout.Close() })
	}()
	go t.read(c)
	return t, nil
}

func (t *stdioTransport) kind() string { return "stdio" }

func (t *stdioTransport) send(_ context.Context, msg []byte, _ int64) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if _, err := t.stdin.Write(msg); err != nil {
		return t.writeErr(err)
	}
	if _, err := t.stdin.Write([]byte{'\n'}); err != nil {
		return t.writeErr(err)
	}
	return nil
}

func (t *stdioTransport) writeErr(err error) error {
	if t.closing.Load() {
		return errClosed
	}
	// A server that exits at once (a bad config, a missing dependency) can
	// close its stdin before the first message is written: then why it
	// exited (exit code, stderr) is the useful error, not "broken pipe".
	select {
	case <-t.exited:
		return t.exitErr()
	case <-time.After(time.Second):
	}
	return fmt.Errorf("write to server: %w", err)
}

// read feeds stdout lines to the connection. Lines may be arbitrarily long.
func (t *stdioTransport) read(c *conn) {
	br := bufio.NewReaderSize(t.stdout, 64<<10)
	var long []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			long = append(long, chunk...)
			continue
		}
		line := chunk
		if len(long) > 0 {
			long = append(long, chunk...)
			line = long
		}
		t.line(c, line)
		if cap(long) > 1<<20 {
			long = nil // don't pin a huge buffer
		} else {
			long = long[:0]
		}
		if err != nil {
			break
		}
	}
	select {
	case <-t.exited:
	case <-time.After(2 * time.Second):
	}
	c.fail(t.exitErr())
}

func (t *stdioTransport) line(c *conn, line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	if line[0] != '{' && line[0] != '[' {
		// Stray output on stdout (servers sometimes log there): keep it
		// out of the protocol but preserve it in the log.
		if t.log != nil { // one write: stderr lines don't come between
			t.log.Write(append(append([]byte("stdout: "), line...), '\n'))
		}
		return
	}
	c.handle(line)
}

func (t *stdioTransport) exitErr() error {
	if t.closing.Load() {
		return errClosed
	}
	var b strings.Builder
	select {
	case <-t.exited:
		b.WriteString("server exited")
		if t.waitErr != nil {
			b.WriteString(" (" + t.waitErr.Error() + ")")
		}
	default:
		b.WriteString("server closed stdout")
	}
	if tail := t.stderrTail(); tail != "" {
		b.WriteString(": " + tail)
	}
	if t.logPath != "" {
		b.WriteString(" (log: " + t.logPath + ")")
	}
	return fmt.Errorf("%s", b.String())
}

// stderrTail returns the last non-empty stderr line written by this run.
func (t *stdioTransport) stderrTail() string {
	if t.log == nil {
		return ""
	}
	if t.copied != nil { // what the server wrote last may still be in the pipe
		select {
		case <-t.copied:
		case <-time.After(500 * time.Millisecond): // a grandchild holds it open
		}
	}
	t.log.sync()
	start := t.log.runStart()
	f, err := os.Open(t.logPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	start = max(start, st.Size()-1024)
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(buf)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" && !strings.HasPrefix(l, "--- ") {
			return truncate(l, 300)
		}
	}
	return ""
}

// close shuts the server down: close stdin, SIGTERM the process group,
// SIGKILL after 2s.
func (t *stdioTransport) close() {
	t.once.Do(func() {
		t.closing.Store(true)
		t.wmu.Lock()
		t.stdin.Close()
		t.wmu.Unlock()
		pgid := t.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-t.exited:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			select {
			case <-t.exited:
			case <-time.After(time.Second):
			}
		}
		// Children that ignored SIGTERM but outlived the leader.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		t.stdout.Close()
		t.closeLog()
	})
}

func (t *stdioTransport) closeLog() {
	if t.stderrW != nil { // not started
		t.stderrW.Close()
	}
	if t.stderr != nil {
		t.stderr.Close() // a grandchild may still hold the other end
	}
	if t.log != nil {
		t.log.Close()
	}
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}
