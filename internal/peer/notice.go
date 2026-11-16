package peer

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Notices: a message with Notify asks its receiver to tell the sender once
// when it next goes idle (its turn ended with nothing left to do) or
// closes. The notice carries the receiver's last answer, so the sender
// needn't ask for it or poll, and the hops of the message that asked: it
// answers that message rather than going on the chain.

// Notice statuses; a turn that failed is "failed: <error>".
const (
	NoticeFinished    = "finished"
	NoticeInterrupted = "interrupted"
	NoticeClosed      = "closed"
)

const (
	noticeTextMax = 12_000 // the answer a notice carries
	maxWatchers   = 16
)

// Watcher is a session waiting for a notice.
type Watcher struct {
	ID   string
	Hops int // of the message that asked
}

// Watchers are the sessions waiting for a session's notice. Safe for
// concurrent use.
type Watchers struct {
	mu   sync.Mutex
	list []Watcher
}

// Add notes m's sender if m asks for a notice and it can take one.
func (w *Watchers) Add(m Message) {
	if !m.Notify || m.Notice != "" || m.From == "" || !m.CanReply {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, x := range w.list {
		if x.ID == m.From {
			w.list[i].Hops = m.Hops
			return
		}
	}
	if len(w.list) >= maxWatchers {
		w.list = w.list[1:]
	}
	w.list = append(w.list, Watcher{ID: m.From, Hops: m.Hops})
}

// Take returns the watchers and forgets them: each hears once.
func (w *Watchers) Take() []Watcher {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.list
	w.list = nil
	return out
}

// String encodes the watchers ("id:hops,…"), for a restarted tego to keep
// them (Set).
func (w *Watchers) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	parts := make([]string, len(w.list))
	for i, x := range w.list {
		parts[i] = x.ID + ":" + strconv.Itoa(x.Hops)
	}
	return strings.Join(parts, ",")
}

// Set adds the watchers String encoded.
func (w *Watchers) Set(s string) {
	for _, part := range strings.Split(s, ",") {
		id, hops, ok := strings.Cut(strings.TrimSpace(part), ":")
		n, err := strconv.Atoi(hops)
		if !ok || id == "" || err != nil {
			continue
		}
		w.Add(Message{From: id, Hops: n, Notify: true, CanReply: true})
	}
}

// Notify sends self's notice to each watcher: status, and answer, self's
// last answer. It returns when they are delivered or after 10 seconds.
func Notify(ctx context.Context, base string, self Self, ws []Watcher, status, answer string) {
	if len(ws) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	answer = strings.TrimSpace(answer)
	if len(answer) > noticeTextMax {
		answer = answer[:noticeTextMax] + "\n[…cut; sessions read shows all of it]"
	}
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = Send(ctx, base, Message{To: w.ID, From: self.ID, FromTitle: self.Title, FromCwd: self.Cwd,
				Text: answer, Hops: w.Hops, Notice: status, Sent: time.Now()})
		}()
	}
	wg.Wait()
}

// noticePrompt is a notice as the receiving model reads it.
func (m Message) noticePrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "<session-notice %s status=%q>\n", m.attrs(), m.Notice)
	if text := strings.TrimSpace(m.Text); text != "" {
		b.WriteString(text + "\n")
	}
	b.WriteString("</session-notice>\n")
	what := "finished its turn and is idle now"
	switch {
	case m.Notice == NoticeInterrupted:
		what = "is idle now: its user interrupted its turn"
	case m.Notice == NoticeClosed:
		what = "was closed: it takes no messages until its user opens it again"
	case strings.HasPrefix(m.Notice, "failed"):
		what = "is idle now: its turn ended with an error"
	}
	fmt.Fprintf(&b, "Session %q, which you asked to hear from, %s. ", m.From, what)
	if strings.TrimSpace(m.Text) != "" {
		b.WriteString("Above is its last answer. ")
	}
	b.WriteString("This notice needs no reply; sessions read shows all it did.")
	return b.String()
}

// noticeLabel is a notice's one-line description for the user.
func (m Message) noticeLabel() string {
	from := m.From
	if m.FromTitle != "" {
		from += " · " + m.FromTitle
	}
	return "session " + from + " " + m.Notice
}
