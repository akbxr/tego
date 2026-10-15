package web

import (
	"os"
	"strings"
	"testing"
)

func TestParseDuckDuckGo(t *testing.T) {
	page, err := os.ReadFile("testdata/ddg.html")
	if err != nil {
		t.Fatal(err)
	}
	hits := parseDuckDuckGo(string(page))
	want := []hit{
		{"Tutorial: Getting started with generics - The Go Programming Language", "https://go.dev/doc/tutorial/generics", "This tutorial introduces the basics of generics in Go."},
		{"Generics in Golang - GeeksforGeeks", "https://www.geeksforgeeks.org/go-language/generics-in-golang/", "Generics are essentially template/boilerplate code"},
		{"Go by Example: Generics & More", "https://gobyexample.com/generics?x=1&y=2", "Go by Example: Generics Next example: Range over Iterators."},
	}
	if len(hits) != len(want) {
		t.Fatalf("got %d hits, want %d: %+v", len(hits), len(want), hits)
	}
	for i, w := range want {
		h := hits[i]
		if h.Title != w.Title || h.URL != w.URL || !strings.HasPrefix(h.Snippet, w.Snippet) {
			t.Errorf("hit %d = %+v\nwant %+v", i, h, w)
		}
	}
	if !strings.Contains(hits[0].Snippet, "you'll declare") {
		t.Errorf("entity not decoded in snippet: %q", hits[0].Snippet)
	}
}

func TestFormatOutcome(t *testing.T) {
	o := &outcome{answer: "Go 1.27 is out.", hits: []hit{{"Go", "https://go.dev", ""}, {"", "https://x.dev", ""}, {"c", "https://c.dev", ""}}}
	got := format("native provider search", "go", o, 2, []string{"Exa: HTTP 500"})
	want := "Web search via native provider search: \"go\"\n(skipped: Exa: HTTP 500)\n\nGo 1.27 is out.\n\nSources:\n1. Go — https://go.dev\n2. https://x.dev — https://x.dev"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
