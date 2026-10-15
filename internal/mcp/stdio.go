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

	log      *os.File // nil when logDir is empty
	logPath  string
	logStart int64
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
			t.logPath = filepath.Join(logDir, "mcp-"+sanitize(cfg.Name)+".log")
			if f, err := os.OpenFile(t.logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
				t.log = f
				if st, err := f.Stat(); err == nil {
					t.logStart = st.Size()
				}
				fmt.Fprintf(f, "--- %s start: %s %s\n", time.Now().Format(time.RFC3339), cfg.Command, strings.Join(cfg.Args, " "))
				cmd.Stderr = f // *os.File: no copy goroutine
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
		if t.log != nil {
			t.log.Write([]byte("stdout: "))
			t.log.Write(line)
			t.log.Write([]byte{'\n'})
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
	if t.logPath == "" {
		return ""
	}
	f, err := os.Open(t.logPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	start := max(t.logStart, st.Size()-1024)
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
