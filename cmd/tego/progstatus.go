package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/akbxr/tego/internal/agent"
)

// Program status (OSC 7501, the Program Status Protocol, revision 0.4:
// https://www.superlogical.com/rex/docs/build/program-status). tego tells
// the terminal it runs in what it is doing, so herdr-core (`hc ls`,
// `hc wait`, `hc get`), herdr, Rex, kitty, tuios, cmux and other readers
// show it without scraping the screen:
//
//   - root record: working while a turn (or a background task) runs,
//     blocked while a tool call waits for approval or a question for an
//     answer, done when a turn finished (until the user acts again), error
//     when it failed, idle otherwise
//   - one child record per running subagent ("agents/<name>"), working,
//     titled with its name, with its latest tool call as the message
//
// Every report carries app=tego. Terminals ignore OSC codes they do not
// know, so reports go out without a feature query. TEGO_PROGRAM_STATUS=0
// (or off) turns them off. The same root state feeds the herdr reporter
// (herdr.go), which herdr versions without OSC 7501 rely on.

const (
	statusApp      = "tego"
	statusMsgMax   = 300 // bytes of decoded msg (the spec allows 2048)
	statusTitleMax = 120 // bytes of decoded title (the spec allows 192)
)

// statusRecord is one record's report; the zero value is no record.
type statusRecord struct {
	state string // idle, working, blocked, done, error
	kind  string // permission, question or auth; blocked only
	title string
	msg   string
}

// encode returns the OSC 7501 report of r for id ("" is the root record).
func (r statusRecord) encode(id string) string {
	var b strings.Builder
	b.WriteString("\x1b]7501;state=" + r.state)
	if id != "" {
		b.WriteString(":id=" + id)
	}
	if r.state != "clear" {
		if r.state == "blocked" && r.kind != "" {
			b.WriteString(":kind=" + r.kind)
		}
		b.WriteString(":app=" + statusApp)
		if t := statusText(r.title, statusTitleMax); t != "" {
			b.WriteString(":title=" + base64.StdEncoding.EncodeToString([]byte(t)))
		}
		if m := statusText(r.msg, statusMsgMax); m != "" {
			b.WriteString(":msg=" + base64.StdEncoding.EncodeToString([]byte(m)))
		}
	}
	b.WriteString("\x1b\\")
	return b.String()
}

// statusText makes s a single line the spec accepts: escapes dropped,
// control characters (which would get the report refused) and direction
// overrides turned into spaces, whitespace collapsed, and cut to max bytes
// on a rune boundary.
func statusText(s string, max int) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r >= 0x7f && r <= 0x9f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return ' '
		case r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := max - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

// statusSegment turns name into an id segment ([A-Za-z0-9_.+-]{1,32}).
func statusSegment(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '+', r == '-':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
		if b.Len() >= 28 { // room for a "-NN" that tells equal names apart
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "agent"
	}
	return s
}

// progStatus keeps tego's records and writes a report whenever one
// changes. Safe for concurrent use: approvals come from agent goroutines.
// A nil *progStatus does nothing.
type progStatus struct {
	mu     sync.Mutex
	write  func(string)       // nil: reports are off, onRoot still runs
	onRoot func(statusRecord) // called with the root after it changes (herdr)
	base   statusRecord       // root as the event loop last set it
	blocks []*statusRecord    // questions waiting for the user, oldest first
	kids   map[string]statusRecord
	sent   map[string]string // id → last report written
	closed bool
}

// newProgStatus returns a reporter writing through write (nothing is
// written when TEGO_PROGRAM_STATUS turns reports off).
func newProgStatus(write func(string)) *progStatus {
	if !programStatusOn() {
		write = nil
	}
	return &progStatus{write: write, sent: map[string]string{}}
}

func programStatusOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TEGO_PROGRAM_STATUS"))) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

// set changes the root record (what the event loop sees: working, idle,
// done or error).
func (p *progStatus) set(r statusRecord) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.base = r
	p.flush()
}

// block reports that tego waits for the user (kind: permission, question
// or auth) until the returned function is called. The latest open
// question is the one shown.
func (p *progStatus) block(kind, msg string) (unblock func()) {
	if p == nil {
		return func() {}
	}
	r := &statusRecord{state: "blocked", kind: kind, msg: msg}
	p.mu.Lock()
	p.blocks = append(p.blocks, r)
	p.flush()
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			for i, b := range p.blocks {
				if b == r {
					p.blocks = append(p.blocks[:i], p.blocks[i+1:]...)
					break
				}
			}
			p.flush()
		})
	}
}

func (p *progStatus) rootLocked() statusRecord {
	if n := len(p.blocks); n > 0 {
		return *p.blocks[n-1]
	}
	return p.base
}

// children replaces the child records (id → record).
func (p *progStatus) children(kids map[string]statusRecord) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kids = kids
	p.flush()
}

// flush writes the reports that changed since the last ones. Called with
// p.mu held, so reports leave in the order their changes happened.
func (p *progStatus) flush() {
	if p.closed {
		return
	}
	var out strings.Builder
	// Children that went away first, then the root, then the rest.
	ids := make([]string, 0, len(p.sent))
	for id := range p.sent {
		if _, ok := p.kids[id]; !ok && id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		out.WriteString(statusRecord{state: "clear"}.encode(id))
		delete(p.sent, id)
	}
	root := p.rootLocked()
	if root.state != "" {
		if seq := root.encode(""); seq != p.sent[""] {
			out.WriteString(seq)
			p.sent[""] = seq
			if p.onRoot != nil {
				p.onRoot(root)
			}
		}
	}
	ids = ids[:0]
	for id := range p.kids {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if seq := p.kids[id].encode(id); seq != p.sent[id] {
			out.WriteString(seq)
			p.sent[id] = seq
		}
	}
	if out.Len() > 0 && p.write != nil {
		p.write(out.String())
	}
}

// close removes every record (the user quit: nothing is left to show)
// and stops reporting.
func (p *progStatus) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && len(p.sent) > 0 && p.write != nil {
		p.write(statusRecord{state: "clear"}.encode(""))
	}
	p.closed = true
}

// finish reports the final state of a run that exits right after (tego
// -p), so the record outlives the process, and stops reporting.
func (p *progStatus) finish(r statusRecord) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.base, p.blocks, p.kids = r, nil, nil
	p.flush()
	p.closed = true
}

// blockStatus reports that tego waits for the user until the returned
// function runs.
func (a *app) blockStatus(kind, msg string) func() {
	return a.pstat.block(kind, msg)
}

// approvalStatus is the message of a blocked report for an approval.
func approvalStatus(req agent.Approval) string {
	what := strings.TrimSpace(req.Summary) // "bash go test ./..." already names the tool
	if what == "" {
		what = req.Tool
	}
	if req.Agent != "" {
		return "Subagent " + req.Agent + ": allow " + what + "?"
	}
	return "Allow " + what + "?"
}

// syncStatus reports the state after whatever the last event changed:
// the root record, one child per running subagent, and herdr's view.
// Event loop only.
func (a *app) syncStatus() {
	if a.pstat == nil {
		return
	}
	var base statusRecord
	switch {
	case a.cancel != nil:
		base = statusRecord{state: "working", msg: a.jobMsg}
	case a.bg != nil && a.bg.Running() > 0:
		// Their reports start a turn of their own: the work is not over.
		base = statusRecord{state: "working", msg: plural(a.bg.Running(), "background task") + " running"}
	default:
		base = a.restStatus
		if base.state == "" {
			base.state = "idle"
		}
	}
	if a.herdr != nil && base.state != "working" { // the agent owns the history while it works
		a.herdr.setSession(a.herdrSession())
	}
	a.pstat.set(base)
	a.pstat.children(a.subagentRecords())
}

// subagentRecords returns a working child record for each subagent that
// runs or waits to: those of the running task call and background tasks.
func (a *app) subagentRecords() map[string]statusRecord {
	var subs []agent.SubagentStatus
	if a.cancel != nil {
		subs = a.ag.Subagents()
	}
	subs = append(subs, a.runningBackground()...)
	kids := map[string]statusRecord{}
	for _, s := range subs {
		if s.State != agent.SubRunning && s.State != agent.SubQueued {
			continue
		}
		seg := s.ID // background tasks: "t1"
		if seg == "" {
			seg = statusSegment(s.Name)
		}
		id := "agents/" + seg
		for n := 2; ; n++ {
			if _, taken := kids[id]; !taken {
				break
			}
			id = fmt.Sprintf("agents/%s-%d", seg, n)
		}
		title := s.Name
		if s.Agent != "" && s.Agent != s.Name {
			title += " (" + s.Agent + ")"
		}
		msg := s.Last
		if s.State == agent.SubQueued {
			msg = "queued"
		} else if msg == "" {
			msg = s.Prompt
		}
		kids[id] = statusRecord{state: "working", title: title, msg: msg}
	}
	return kids
}

// endStatus sets the root record a finished job leaves while tego waits
// for the next prompt: done with the start of the answer, error with the
// error, or idle when the user interrupted it.
func (a *app) endStatus(err error, interrupted bool) {
	switch {
	case interrupted:
		a.restStatus = statusRecord{state: "idle"}
	case err != nil:
		a.restStatus = statusRecord{state: "error", msg: err.Error()}
	default:
		a.restStatus = statusRecord{state: "done", msg: answerLine(a.ag.LastText())}
	}
}

// answerLine returns the first non-blank line of s, without markdown
// heading and emphasis marks.
func answerLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#>*-` "))
		l = strings.ReplaceAll(l, "**", "")
		if l != "" {
			return l
		}
	}
	return ""
}

// herdrState maps a root record onto herdr's idle, working and blocked.
func herdrState(r statusRecord) (state, msg string) {
	switch r.state {
	case "working":
		return "working", ""
	case "blocked":
		return "blocked", statusText(r.msg, statusMsgMax)
	}
	return "idle", ""
}
