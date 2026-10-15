package main

import (
	"reflect"
	"testing"

	"github.com/akbxr/tego/internal/lineedit"
)

func TestExtractMentions(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"fix @src/main.go please", []string{"src/main.go"}},
		{"compare @a.go and @b.go.", []string{"a.go", "b.go"}},                            // trailing period stripped
		{`look at @"docs/my notes.md" now`, []string{"docs/my notes.md"}},                 // quoted with spaces
		{"see (@cmd/tego/main.go), then @cmd/tego/main.go", []string{"cmd/tego/main.go"}}, // bracket lead, dedupe
		{"mail me at user@example.com", nil},                                              // not after whitespace
		{"@README.md", []string{"README.md"}},                                             // start of text
	}
	for _, c := range cases {
		if got := extractMentions(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("extractMentions(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSuggestFilesReplacesOnlyLastToken(t *testing.T) {
	a := &app{}
	a.files.items = []lineedit.PickItem{{Label: "cmd/"}, {Label: "main.go"}, {Label: "cmd/tego/main.go"}, {Label: "my notes.md"}}
	a.files.top = []lineedit.PickItem{{Label: "cmd/"}, {Label: "main.go"}}

	// Enter only completes (not Final) and the trailing space ends the token.
	got := a.suggestFiles("fix @mai")
	if len(got) == 0 || got[0].Value != "fix @main.go " || got[0].Final {
		t.Fatalf("file completion: %+v", got)
	}
	if got := a.suggestFiles("see @cm"); len(got) == 0 || got[0].Value != "see @cmd/" || got[0].Description != "directory" {
		t.Fatalf("directory completion should not add a space: %+v", got)
	}
	if got := a.suggestFiles("open @notes"); len(got) != 1 || got[0].Value != `open @"my notes.md" ` {
		t.Fatalf("paths with spaces are quoted: %+v", got)
	}
	if got := a.suggestFiles("@"); len(got) != 2 {
		t.Fatalf("bare @ lists top-level entries: %+v", got)
	}
	if got := a.suggestFiles("no mention here"); got != nil {
		t.Fatalf("no popup without @ token: %+v", got)
	}
}
