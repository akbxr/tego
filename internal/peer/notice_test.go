package peer

import (
	"context"
	"strings"
	"testing"
)

// Watchers note only senders that asked and can take a notice, once each,
// and survive a restart through String and Set.
func TestWatchers(t *testing.T) {
	var w Watchers
	w.Add(Message{From: "a", Hops: 1, Notify: true, CanReply: true})
	w.Add(Message{From: "a", Hops: 3, Notify: true, CanReply: true}) // again: the later hops
	w.Add(Message{From: "b", Hops: 1, CanReply: true})               // didn't ask
	w.Add(Message{From: "c", Hops: 1, Notify: true})                 // can't take one
	w.Add(Message{From: "d", Hops: 2, Notify: true, CanReply: true, Notice: NoticeFinished})
	if s := w.String(); s != "a:3" {
		t.Fatalf("watchers %q", s)
	}
	var r Watchers
	r.Set(w.String() + ",bad,e:2")
	if got := r.Take(); len(got) != 2 || got[0] != (Watcher{"a", 3}) || got[1] != (Watcher{"e", 2}) {
		t.Fatalf("restored %+v", got)
	}
	if r.Take() != nil {
		t.Fatal("a watcher hears twice")
	}
}

// A notice reaches the watcher with the status, the answer and the hops of
// the message that asked; the model reads it as needing no reply.
func TestNotify(t *testing.T) {
	base := t.TempDir()
	h := &fakeHandler{open: []Open{{ID: "20261116-100000-aaaaaa"}}}
	hub, err := Listen(base, h)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	Notify(context.Background(), base, Self{ID: "20261116-110000-bbbbbb", Title: "worker"},
		[]Watcher{{ID: "20261116-100000-aaaaaa", Hops: 2}}, NoticeFinished, "  all tests pass  ")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.got) != 1 {
		t.Fatalf("got %+v", h.got)
	}
	m := h.got[0]
	if m.Notice != NoticeFinished || m.Text != "all tests pass" || m.Hops != 2 || m.From != "20261116-110000-bbbbbb" {
		t.Fatalf("notice %+v", m)
	}
	p := m.Prompt()
	for _, want := range []string{`<session-notice from="20261116-110000-bbbbbb" title="worker" status="finished">`, "all tests pass",
		"finished its turn and is idle now", "needs no reply"} {
		if !strings.Contains(p, want) {
			t.Fatalf("no %q in:\n%s", want, p)
		}
	}
	if l := m.Label(); l != "session 20261116-110000-bbbbbb · worker finished" {
		t.Fatalf("label %q", l)
	}
	closed := Message{From: "x", Notice: NoticeClosed}
	if p := closed.Prompt(); !strings.Contains(p, "was closed") || strings.Contains(p, "Above is its last answer") {
		t.Fatalf("closed:\n%s", p)
	}
}

// A message that asks for a notice tells its receiver not to send its
// answer; session_send says what each outcome means.
func TestSendNotify(t *testing.T) {
	m := Message{From: "s1", Text: "run the tests", Notify: true, CanReply: true}
	if p := m.Prompt(); !strings.Contains(p, "gets your last answer when your turn ends") || strings.Contains(p, "If it needs an answer") {
		t.Fatalf("prompt:\n%s", p)
	}
	target := Open{ID: "s2", Title: "tests"}
	for status, want := range map[string]string{
		"watching": "Watching s2 (tests): it is working",
		"idle":     "s2 (tests) is idle now, so there is no turn to wait for. Its last answer:\n\nok",
		"started":  "started a turn to answer. When it next goes idle or closes, you get a notice",
	} {
		out, err := delivered(target, Outcome{Status: status, Note: "ok"}, true, true)
		if status == "started" {
			out, err = delivered(target, Outcome{Status: status}, true, true)
		}
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("%s: %q %v", status, out, err)
		}
	}
	send := &SendTool{Base: t.TempDir(), Self: func() Self { return Self{ID: "s1"} }}
	if _, err := send.Run(context.Background(), []byte(`{"id":"s2","notify":true}`)); err == nil || !strings.Contains(err.Error(), "can't take a notice") {
		t.Fatalf("notify without taking messages: %v", err)
	}
	if _, err := send.Run(context.Background(), []byte(`{"id":"s2"}`)); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("no text, no notify: %v", err)
	}
}
