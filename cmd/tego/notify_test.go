package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestNotifySequence(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("KITTY_WINDOW_ID", "1")
	seq := notifySequence("Fix login; flow", "Complete")
	b64 := base64.StdEncoding.EncodeToString
	if !strings.HasPrefix(seq, "\x1b]99;i=tego-") || !strings.Contains(seq, ":d=0:a=focus:e=1;"+b64([]byte("Fix login; flow"))+"\x1b\\") ||
		!strings.Contains(seq, ":p=body:e=1;"+b64([]byte("Complete"))+"\x1b\\") {
		t.Fatalf("kitty sequence %q", seq)
	}
	t.Setenv("KITTY_WINDOW_ID", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TERM_PROGRAM", "WezTerm")
	if seq := notifySequence("t", "Complete"); seq != "\x1b]9;t: Complete\x07" {
		t.Fatalf("OSC 9 sequence %q", seq)
	}
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	if seq := notifySequence("t", "Complete"); seq != "\a" {
		t.Fatalf("fallback %q", seq)
	}
}
