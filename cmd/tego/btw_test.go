package main

import "testing"

func TestBtwArgs(t *testing.T) {
	for line, want := range map[string]string{"/btw why?": "why?", "/btw   ": "", "/btw": ""} {
		if q, ok := btwArgs(line); !ok || q != want {
			t.Errorf("%q: %q %v", line, q, ok)
		}
	}
	for _, line := range []string{"/btwx a", "btw a", "/tree"} {
		if _, ok := btwArgs(line); ok {
			t.Errorf("%q taken for /btw", line)
		}
	}
}
