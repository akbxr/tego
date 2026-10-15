package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"sync/atomic"
)

// Completion notifications: when a turn finishes (and nothing is
// queued), tego notifies the terminal it runs in.
//   - inside herdr: herdr notifies from the reported state in background
//     tabs; in the active tab (where herdr stays quiet) tego runs
//     `herdr notification show … --sound done`
//   - kitty: an OSC 99 desktop notification
//   - iTerm2, WezTerm, Ghostty: an OSC 9 notification
//   - anything else: the terminal bell
// "notify": "off" in config.json, or TEGO_NOTIFY=off, turns it off.

var notifySeq atomic.Int64

func (a *app) notifyDone() {
	if a.cfg.Notify == "off" || a.tui == nil || os.Getenv("TEGO_NOTIFY") == "off" {
		return
	}
	title := "tego"
	if s := a.ag.Session; s != nil && s.Title != "" {
		title = s.Title
	}
	const body = "Complete"
	if a.herdr != nil {
		a.herdr.notify(title, body)
		return
	}
	a.tui.WriteRaw(notifySequence(title, body))
}

// notifySequence picks the notification escape the terminal understands.
func notifySequence(title, body string) string {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	switch tp := os.Getenv("TERM_PROGRAM"); {
	case os.Getenv("KITTY_WINDOW_ID") != "" || os.Getenv("TERM") == "xterm-kitty":
		id := fmt.Sprintf("tego-%d", notifySeq.Add(1))
		// Title then body, base64 (e=1) so any text is safe; a=focus brings
		// the window forward when the notification is clicked.
		return fmt.Sprintf("\x1b]99;i=%s:d=0:a=focus:e=1;%s\x1b\\\x1b]99;i=%s:p=body:e=1;%s\x1b\\", id, b64(title), id, b64(body))
	case tp == "iTerm.app" || tp == "WezTerm" || tp == "ghostty":
		return "\x1b]9;" + title + ": " + body + "\x07"
	}
	return "\a"
}
