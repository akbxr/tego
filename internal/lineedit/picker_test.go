package lineedit

import (
	"reflect"
	"testing"
)

func TestFilterItems(t *testing.T) {
	items := []PickItem{
		{Label: "anthropic/claude-opus-5-5"},
		{Label: "anthropic/claude-sonnet-5-5", Detail: "current"},
		{Label: "openai-codex/gpt-5.6"},
		{Label: "openrouter/anthropic/claude-sonnet-4.5"},
	}
	cases := []struct {
		query string
		want  []int
	}{
		{"", []int{0, 1, 2, 3}},
		// substring hits rank by position; earlier is better
		{"sonnet", []int{1, 3}},
		// every term must match; subsequence matches rank after substrings
		{"anth son", []int{1, 3}},
		{"gpt", []int{2}},
		{"cdx", []int{2}}, // subsequence of "codex"
		{"CURRENT", []int{1}},
		{"zzz", []int{}},
	}
	for _, c := range cases {
		if got := Filter(items, c.query); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Filter(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

func TestPickerMoveScrollsAndWraps(t *testing.T) {
	p := &picker{items: make([]PickItem, 10)}
	p.refilter(-1)
	p.move(-1, 3) // wrap to last
	if p.cursor != 9 || p.offset != 7 {
		t.Fatalf("wrap up: cursor=%d offset=%d", p.cursor, p.offset)
	}
	p.move(1, 3) // wrap to first
	if p.cursor != 0 || p.offset != 0 {
		t.Fatalf("wrap down: cursor=%d offset=%d", p.cursor, p.offset)
	}
	p.refilter(5)
	p.move(0, 3)
	if p.cursor != 5 || p.offset != 3 {
		t.Fatalf("initial selection visible: cursor=%d offset=%d", p.cursor, p.offset)
	}
}
