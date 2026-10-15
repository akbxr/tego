package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// herdr integration (https://herdr.dev/docs/add-herdr-support/). Inside a
// herdr pane, interactive tego reports its state (working while a turn
// runs, idle otherwise) and the command that resumes the current session,
// and releases the pane on exit. herdr then shows tego in its sidebar and
// `herdr agent list`, notifies when a turn finishes in a background tab, and
// reopens the session after a herdr restart.
//
// Reports go through "$HERDR_BIN_PATH" in a background goroutine with a
// timeout; only the latest pending report is sent and failures are ignored,
// so herdr never slows tego down.

const herdrSource = "tego" // integration id; also the agent name users see

type herdrReporter struct {
	bin, pane string

	mu      sync.Mutex
	seq     int64    // last sequence number handed out
	last    string   // key of the last queued report, to skip repeats
	pending []string // latest report not sent yet
	wake    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
}

// newHerdr returns a reporter when tego runs in a herdr pane, else nil.
func newHerdr() *herdrReporter {
	h := &herdrReporter{bin: os.Getenv("HERDR_BIN_PATH"), pane: os.Getenv("HERDR_PANE_ID")}
	if os.Getenv("HERDR_ENV") != "1" || h.bin == "" || h.pane == "" || os.Getenv("HERDR_SOCKET_PATH") == "" {
		return nil
	}
	h.wake, h.stop, h.stopped = make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
	go h.loop()
	return h
}

// nextSeq returns an increasing number that also stays above the numbers
// of earlier tego processes: microseconds since the epoch.
func (h *herdrReporter) nextSeq() string {
	n := time.Now().UnixMicro()
	if n <= h.seq {
		n = h.seq + 1
	}
	h.seq = n
	return strconv.FormatInt(n, 10)
}

// herdrSession identifies the session to resume; zero when there is none
// worth resuming.
type herdrSession struct {
	id, path string
	resume   []string
}

// report queues state ("working" or "idle") with the session, unless it
// repeats the previous report.
func (h *herdrReporter) report(state string, s herdrSession) {
	if h == nil {
		return
	}
	key := state + "\x00" + s.id + "\x00" + strings.Join(s.resume, "\x00")
	h.mu.Lock()
	defer h.mu.Unlock()
	if key == h.last {
		return
	}
	h.last = key
	args := []string{"pane", "report-agent", h.pane, "--source", herdrSource, "--agent", herdrSource, "--state", state, "--seq", h.nextSeq()}
	if s.id != "" {
		args = append(args, "--agent-session-id", s.id, "--agent-session-path", s.path)
		if len(s.resume) > 0 {
			args = append(append(args, "--"), s.resume...)
		}
	}
	h.pending = args
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *herdrReporter) loop() {
	defer close(h.stopped)
	for {
		select {
		case <-h.stop:
			return
		case <-h.wake:
		}
		h.mu.Lock()
		args := h.pending
		h.pending = nil
		h.mu.Unlock()
		if args != nil {
			h.run(3*time.Second, args...)
		}
	}
}

func (h *herdrReporter) run(timeout time.Duration, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, h.bin, args...).Output()
	return out
}

// close drops pending reports and releases the pane, so herdr clears
// tego's name, state and resume command right away.
func (h *herdrReporter) close() {
	if h == nil {
		return
	}
	close(h.stop)
	<-h.stopped
	h.mu.Lock()
	seq := h.nextSeq()
	h.mu.Unlock()
	h.run(2*time.Second, "pane", "release-agent", h.pane, "--source", herdrSource, "--agent", herdrSource, "--seq", seq)
}

// tabFocused reports whether the pane's tab is the active one. herdr
// suppresses its own finish notification there, so tego sends one itself.
func (h *herdrReporter) tabFocused() bool {
	var pane struct {
		Result struct {
			Pane struct {
				TabID string `json:"tab_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if json.Unmarshal(h.run(2*time.Second, "pane", "get", h.pane), &pane) != nil || pane.Result.Pane.TabID == "" {
		return true // unknown: better a notification than none
	}
	var tab struct {
		Result struct {
			Tab struct {
				Focused bool `json:"focused"`
			} `json:"tab"`
		} `json:"result"`
	}
	if json.Unmarshal(h.run(2*time.Second, "tab", "get", pane.Result.Pane.TabID), &tab) != nil {
		return true
	}
	return tab.Result.Tab.Focused
}

// notify shows a finish notification with herdr's "done" sound when the
// pane's tab is active; in a background tab herdr notifies on its own from
// the reported state.
func (h *herdrReporter) notify(title, body string) {
	go func() {
		if h.tabFocused() {
			h.run(5*time.Second, "notification", "show", title, "--body", body, "--sound", "done")
		}
	}()
}

// syncHerdr reports the current state. While a turn runs only the state
// is sent: the agent goroutine owns the history then.
func (a *app) syncHerdr() {
	if a.herdr == nil {
		return
	}
	if a.cancel != nil {
		a.herdr.report("working", herdrSession{})
		return
	}
	a.herdr.report("idle", a.herdrSession())
}

// herdrSession describes the current session and the command that reopens
// it with the same model and thinking level. A session with no messages
// has no file to resume yet, and the command is left out when the binary
// is not on PATH under the name it was started with.
func (a *app) herdrSession() herdrSession {
	s := a.ag.Session
	if s == nil || len(a.ag.Messages) == 0 {
		return herdrSession{}
	}
	hs := herdrSession{id: s.ID, path: s.Path}
	name := filepath.Base(os.Args[0])
	if _, err := exec.LookPath(name); err != nil {
		return hs
	}
	argv := []string{name, "-r", s.ID}
	if a.ag.Provider != nil {
		argv = append(argv, "-m", a.ag.ProviderName+"/"+a.ag.Model)
	}
	if a.ag.Thinking != "" {
		argv = append(argv, "-t", a.ag.Thinking)
	}
	for _, arg := range argv {
		if strings.ContainsFunc(arg, func(r rune) bool { return r == '\'' || r < 0x20 || r == 0x7f }) {
			return hs // herdr rejects such a command
		}
	}
	hs.resume = argv
	return hs
}
