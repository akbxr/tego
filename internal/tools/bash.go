package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

const (
	bashDefaultTimeout = 120
	bashMaxTimeout     = 600
	outHeadLines       = 200
	outTailLines       = 300
	outHeadBytes       = 10 << 10
	outTailBytes       = 20 << 10
)

type bashTool struct{ env *Env }

func (t *bashTool) Def() llm.Tool {
	return llm.Tool{
		Name: "bash",
		Description: "Run a shell command with bash -c in the working directory; returns combined stdout+stderr " +
			"(long output is truncated in the middle). No stdin: avoid interactive commands. Default timeout 120s, max 600s. " +
			"For something that keeps running (a dev server, a watcher, a long build) set background: true: it returns at once with an id " +
			"for bash_output, and the process keeps running until it ends, you kill it, or tego exits. Don't end commands with & yourself. " +
			"To view a file use the read tool, not cat/head/sed.",
		Schema: schema(`{"type":"object","properties":{` +
			`"command":{"type":"string"},` +
			`"timeout":{"type":"number","description":"Seconds (default 120, max 600); ignored with background"},` +
			`"background":{"type":"boolean","description":"Keep running after this call; read it with bash_output"}},` +
			`"required":["command"]}`),
	}
}

func (t *bashTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Command    string  `json:"command"`
		Timeout    float64 `json:"timeout"`
		Background bool    `json:"background"`
	}
	if err := decode(input, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Command) == "" {
		return "", errors.New("command is required")
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = bashDefaultTimeout
	}
	if timeout > bashMaxTimeout {
		timeout = bashMaxTimeout
	}
	if why := t.env.blocked(in.Command); why != "" {
		return "", fmt.Errorf("tego blocked this command because %s. The agent never runs it; if it is really needed, "+
			"explain why to the user and let them run it themselves (in tego: !<command>)", why)
	}
	if in.Background {
		out, err := t.env.startBackground(in.Command)
		return out + t.env.noteShellWrite(in.Command), err
	}
	dur := time.Duration(timeout * float64(time.Second))
	out, err := runShell(ctx, t.env.Cwd, in.Command, dur)
	return out + t.env.noteShellWrite(in.Command), err
}

func runShell(ctx context.Context, dir, command string, timeout time.Duration) (string, error) {
	shell := "bash"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "sh"
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(tctx, shell, "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=dumb", "PAGER=cat", "GIT_PAGER=cat", "NO_COLOR=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the whole process group, not just the shell.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Background children that keep the output pipe open must not block us
	// forever once the shell itself has exited.
	cmd.WaitDelay = 2 * time.Second
	out := &capWriter{}
	var w io.Writer = out
	if fn := progressFrom(ctx); fn != nil {
		w = io.MultiWriter(out, progressWriter(fn)) // shown live while it runs
	}
	cmd.Stdout = w
	cmd.Stderr = w // same writer: exec shares one pipe for both, in order

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start %s: %v", shell, err)
	}
	werr := cmd.Wait()

	text := out.String()
	var notes []string
	switch {
	case ctx.Err() != nil:
		notes = append(notes, "[aborted]")
	case errors.Is(tctx.Err(), context.DeadlineExceeded):
		notes = append(notes, fmt.Sprintf("[timed out after %ds]", int(time.Since(start).Round(time.Second)/time.Second)))
	default:
		var ee *exec.ExitError
		if errors.As(werr, &ee) && ee.ExitCode() != 0 {
			if ee.ExitCode() < 0 {
				notes = append(notes, "[killed: "+ee.String()+"]")
			} else {
				notes = append(notes, fmt.Sprintf("[exit code %d]", ee.ExitCode()))
			}
		}
	}
	if len(notes) > 0 {
		if text != "" {
			text += "\n"
		}
		text += strings.Join(notes, " ")
	}
	if text == "" {
		text = "(no output)"
	}
	return text, nil
}

// capWriter keeps the first outHeadLines/outHeadBytes and the last
// outTailBytes of a stream, counting everything else.
type capWriter struct {
	mu        sync.Mutex
	head      []byte
	headLines int
	headDone  bool
	tail      []byte
	total     int64 // bytes written
	lines     int   // newlines written
	tailFrom  int64 // stream offset of tail[0]
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.lines += bytes.Count(p, []byte{'\n'})
	for !w.headDone && len(p) > 0 {
		room := outHeadBytes - len(w.head)
		i := bytes.IndexByte(p, '\n')
		if i < 0 || i+1 > room {
			take := len(p)
			if take > room {
				take = room
			}
			w.head = append(w.head, p[:take]...)
			w.total += int64(take)
			p = p[take:]
			if len(w.head) >= outHeadBytes {
				w.headDone = true
			}
			break
		}
		w.head = append(w.head, p[:i+1]...)
		w.total += int64(i + 1)
		p = p[i+1:]
		w.headLines++
		if w.headLines >= outHeadLines {
			w.headDone = true
		}
	}
	if len(p) > 0 {
		if len(w.tail) == 0 {
			w.tailFrom = w.total
		}
		w.tail = append(w.tail, p...)
		w.total += int64(len(p))
		if len(w.tail) > 2*outTailBytes {
			drop := len(w.tail) - outTailBytes
			w.tailFrom += int64(drop)
			w.tail = append(w.tail[:0], w.tail[drop:]...)
		}
	}
	return n, nil
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	head := string(w.head)
	tail := w.tail
	complete := w.tailFrom == int64(len(w.head)) || len(tail) == 0
	if len(tail) > outTailBytes {
		tail = tail[len(tail)-outTailBytes:]
		complete = false
	}
	if !complete {
		// Start the tail at a line boundary.
		if i := bytes.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
			tail = tail[i+1:]
		}
	}
	if c := bytes.Count(tail, []byte{'\n'}); c > outTailLines {
		for range c - outTailLines {
			tail = tail[bytes.IndexByte(tail, '\n')+1:]
		}
		complete = false
	}
	if complete {
		return strings.ToValidUTF8(strings.TrimRight(head+string(tail), "\n"), "\uFFFD")
	}
	tailLines := bytes.Count(tail, []byte{'\n'})
	if len(tail) > 0 && tail[len(tail)-1] != '\n' {
		tailLines++
	}
	headLines := w.headLines
	if len(w.head) > 0 && w.head[len(w.head)-1] != '\n' {
		headLines++
	}
	totalLines := w.lines
	if w.total > 0 && !strings.HasSuffix(head+string(tail), "\n") {
		totalLines++
	}
	omitted := totalLines - headLines - tailLines
	marker := "\n... output truncated ...\n"
	if omitted > 0 {
		marker = fmt.Sprintf("\n... %d lines omitted ...\n", omitted)
	}
	return strings.ToValidUTF8(strings.TrimRight(head, "\n")+marker+strings.TrimRight(string(tail), "\n"), "\uFFFD")
}
