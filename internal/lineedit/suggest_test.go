package lineedit

import (
	"strings"
	"testing"
)

var testCmds = []Suggestion{
	{Value: "/model", Label: "model", Description: "Switch model", Final: true},
	{Value: "/quit", Label: "quit", Description: "Exit", Final: true},
	{Value: "/skill:", Label: "skill:", Description: "Run a skill"},
}

// prefixSuggest suggests the items whose Value has line as a prefix.
func prefixSuggest(items []Suggestion) func(string) []Suggestion {
	return func(line string) []Suggestion {
		if !strings.HasPrefix(line, "/") {
			return nil
		}
		var out []Suggestion
		for _, it := range items {
			if strings.HasPrefix(it.Value, line) && it.Value != line {
				out = append(out, it)
			}
		}
		return out
	}
}

func suggestTUI(items []Suggestion, w, h int) *testTUI {
	return newTestTUI(Options{
		Status:  func(int) []string { return []string{"st"} },
		Suggest: prefixSuggest(items),
	}, w, h)
}

func checkFooter(t *testing.T, tt *testTUI, want ...string) {
	t.Helper()
	if got := tt.emu().all(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("footer %q\nwant   %q", got, want)
	}
}

func TestSuggestPopupShowsAndFilters(t *testing.T) {
	tt := suggestTUI(testCmds, 40, 20)
	checkFooter(t, tt, "", ">", "st")
	if evs := tt.feed("/"); len(evs) != 0 {
		t.Fatalf("events: %+v", evs)
	}
	checkFooter(t, tt, "", "> /", "› model   Switch model", "  quit    Exit", "  skill:  Run a skill", "st")
	if e := tt.emu(); e.row != 1 || e.col != 3 {
		t.Fatalf("cursor (%d,%d) want (1,3)", e.row, e.col)
	}
	if got := tt.ContentHeight(); got != 20-5 {
		t.Fatalf("ContentHeight = %d", got)
	}
	tt.feed("q")
	checkFooter(t, tt, "", "> /q", "› quit  Exit", "st")
	tt.feed("x")
	checkFooter(t, tt, "", "> /qx", "st")
	// Backspace re-queries; the selection stays on /quit.
	tt.feed("\x7f\x7f")
	checkFooter(t, tt, "", "> /", "  model   Switch model", "› quit    Exit", "  skill:  Run a skill", "st")
}

func TestSuggestSelection(t *testing.T) {
	tt := suggestTUI(testCmds, 40, 20)
	tt.feed("/\x1b[B")
	checkFooter(t, tt, "", "> /", "  model   Switch model", "› quit    Exit", "  skill:  Run a skill", "st")
	tt.feed("\x1b[A\x10") // Up, Ctrl+P: wraps to the last item
	checkFooter(t, tt, "", "> /", "  model   Switch model", "  quit    Exit", "› skill:  Run a skill", "st")
	tt.feed("\x0e") // Ctrl+N wraps to the first
	checkFooter(t, tt, "", "> /", "› model   Switch model", "  quit    Exit", "  skill:  Run a skill", "st")
	// The selection follows its Value across re-queries.
	tt.feed("\x1b[B\x1b[B")
	tt.feed("s")
	checkFooter(t, tt, "", "> /s", "› skill:  Run a skill", "st")
	tt.feed("\x7f")
	checkFooter(t, tt, "", "> /", "  model   Switch model", "  quit    Exit", "› skill:  Run a skill", "st")
}

func TestSuggestScrolls(t *testing.T) {
	var items []Suggestion
	for i := range 15 {
		v := string(rune('a'+i/10)) + string(rune('0'+i%10))
		items = append(items, Suggestion{Value: "/" + v, Label: v, Final: true})
	}
	tt := suggestTUI(items, 30, 30)
	tt.feed("/")
	rows := func(first, sel int) []string {
		out := []string{"", "> /"}
		for i := first; i < first+maxSuggestRows; i++ {
			p := "  "
			if i == sel {
				p = "› "
			}
			out = append(out, p+items[i].Label)
		}
		return append(out, "st")
	}
	checkFooter(t, tt, rows(0, 0)...)
	tt.feed(strings.Repeat("\x1b[B", 11))
	checkFooter(t, tt, rows(2, 11)...)
	tt.feed(strings.Repeat("\x1b[A", 11)) // back to 0, then wraps to 14
	checkFooter(t, tt, rows(0, 0)...)
	tt.feed("\x1b[A")
	checkFooter(t, tt, rows(5, 14)...)
	if evs := tt.feed("\r"); len(evs) != 1 || evs[0] != (Event{Kind: EvSubmit, Text: "/b4"}) {
		t.Fatalf("enter: %+v", evs)
	}
}

func TestSuggestTabAndEnter(t *testing.T) {
	skills := append(testCmds[:len(testCmds):len(testCmds)],
		Suggestion{Value: "/skill:review", Label: "skill:review", Final: true})
	for _, tc := range []struct{ in, want string }{
		{"/mo\tgpt\r", "/model gpt"}, // Tab on Final adds a space for arguments
		{"/qu\r", "/quit"},           // Enter on Final submits Value
		{"/sk\t\r", "/skill:review"}, // Tab on a prefix completes without space
		{"/sk\r\r", "/skill:review"}, // Enter on non-Final completes only
		{"a\tb\r", "a\tb"},           // no popup: Tab is a tab
		{"/zz\t\r", "/zz"},           // no popup on a slash line: Tab is ignored
	} {
		if got := submitted(t, suggestTUI(skills, 60, 20), tc.in); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}
	tt := suggestTUI(skills, 60, 20)
	if evs := tt.feed("/sk\r"); len(evs) != 0 {
		t.Fatalf("non-final enter emitted %+v", evs)
	}
	checkFooter(t, tt, "", "> /skill:", "› skill:review", "st")
	// Ending in ':' gets no trailing space even when Final.
	tt = suggestTUI([]Suggestion{{Value: "/x:", Label: "x:", Final: true}}, 60, 20)
	tt.feed("/\t")
	checkFooter(t, tt, "", "> /x:", "st")
}

// Completions query the text before the cursor and keep what follows it.
func TestSuggestAtCursor(t *testing.T) {
	var queries []string
	fn := func(before string) []Suggestion {
		queries = append(queries, before)
		if strings.HasSuffix(before, " @a") {
			return []Suggestion{{Value: strings.TrimSuffix(before, "@a") + "@alpha.go ", Label: "alpha.go"}}
		}
		return nil
	}
	tt := newTestTUI(Options{Status: func(int) []string { return []string{"st"} }, Suggest: fn}, 60, 20)
	// Type "see  then go", move the cursor back between the two spaces, type "@a".
	tt.feed("see  then go" + strings.Repeat("\x1b[D", 8) + "@a")
	if q := queries[len(queries)-1]; q != "see @a" {
		t.Fatalf("query = %q", q)
	}
	if got := submitted(t, tt, "\t\r"); got != "see @alpha.go then go" {
		t.Fatalf("submitted %q", got)
	}
	// A token on a later line pops up too.
	queries = nil
	tt = newTestTUI(Options{Suggest: fn}, 60, 20)
	tt.feed("first\x0ase @a")
	if q := queries[len(queries)-1]; q != "first\nse @a" {
		t.Fatalf("multi-line query = %q", q)
	}
}

func TestSuggestEscape(t *testing.T) {
	tt := suggestTUI(testCmds, 40, 20)
	if evs := tt.feed("/\x1b"); len(evs) != 0 {
		t.Fatalf("esc with popup emitted %+v", evs)
	}
	checkFooter(t, tt, "", "> /", "st")
	// Hidden popup: Up/Down/Enter behave normally.
	if evs := tt.feed("\x1b"); len(evs) != 1 || evs[0].Kind != EvEscape {
		t.Fatalf("esc without popup: %+v", evs)
	}
	tt.feed("m")
	checkFooter(t, tt, "", "> /m", "› model  Switch model", "st")
	tt.feed("\x1b")
	if evs := tt.feed("\r"); len(evs) != 1 || evs[0] != (Event{Kind: EvSubmit, Text: "/m"}) {
		t.Fatalf("enter after esc: %+v", evs)
	}
}

func TestSuggestHistoryRecall(t *testing.T) {
	tt := suggestTUI(testCmds, 40, 20)
	tt.AddHistory("first")
	tt.AddHistory("/mo")
	tt.feed("\x1b[A")
	checkFooter(t, tt, "", "> /mo", "st")
	if got := submitted(t, tt, "\x1b[A\r"); got != "first" {
		t.Fatalf("history: %q", got)
	}
}

func TestSuggestFooterLayout(t *testing.T) {
	tt := suggestTUI(testCmds, 40, 20)
	tt.feed("/")
	footer := []string{"> /", "› model   Switch model", "  quit    Exit", "  skill:  Run a skill", "st"}
	content := "line one\nline \x1b[1mtwo\x1b[0m\npartial"
	for _, ch := range []byte(content) {
		_, _ = tt.Writer().Write([]byte{ch})
	}
	checkLayout(t, tt, content, footer)
	if e := tt.emu(); e.row != 3 || e.col != 3 {
		t.Fatalf("cursor (%d,%d) want (3,3)", e.row, e.col)
	}
	// Closing the popup shrinks the footer without leaving garbage.
	tt.feed("\x1b")
	checkLayout(t, tt, content, []string{"> /", "st"})
	if got := tt.ContentHeight(); got != 20-2 {
		t.Fatalf("ContentHeight = %d", got)
	}
}

func TestSuggestHiddenWhilePicking(t *testing.T) {
	tt := suggestTUI(testCmds, 50, 30)
	tt.feed("/")
	ch := make(chan int, 1)
	go func() {
		idx, _ := tt.Pick("Choose", []PickItem{{Label: "alpha"}}, "", 0)
		ch <- idx
	}()
	waitPick(t, tt)
	checkFooter(t, tt, "", "Choose", "›", "❯ alpha", "  1/1 · ↑↓ move · enter select · esc cancel", "st")
	tt.feed("\r")
	if idx := <-ch; idx != 0 {
		t.Fatalf("pick = %d", idx)
	}
	checkFooter(t, tt, "", "> /", "› model   Switch model", "  quit    Exit", "  skill:  Run a skill", "st")
}

func TestSetInput(t *testing.T) {
	tt := suggestTUI(testCmds, 40, 20)
	tt.feed("draft")
	tt.SetInput("edited prompt")
	checkFooter(t, tt, "", "> edited prompt", "st")
	if e := tt.emu(); e.row != 1 || e.col != 15 {
		t.Fatalf("cursor (%d,%d) want (1,15)", e.row, e.col)
	}
	if got := submitted(t, tt, "!\r"); got != "edited prompt!" {
		t.Fatalf("submit: %q", got)
	}
	tt.SetInput("/q")
	checkFooter(t, tt, "", "> /q", "› quit  Exit", "st")
}

func TestSuggestRender(t *testing.T) {
	s := suggestState{items: []Suggestion{
		{Label: "short", Icon: "◆", Description: "first"},
		{Label: strings.Repeat("l", 40), Description: "long label"},
		// Too long for one row: wraps at a space, the rest is cut with "…".
		{Label: "wrap", Description: "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu"},
	}, sel: 1}
	plain := func(string) string { return "" }
	got := s.appendLines(nil, 10, 60, plain)
	want := []string{
		"  ◆ short                 first",
		"›   " + strings.Repeat("l", 19) + "…  long label",
		"    wrap                  alpha beta gamma delta epsilon",
		"                          zeta eta theta iota kappa lambda…",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows %q\nwant %q", got, want)
	}
	for _, l := range got {
		if w := visibleWidth(l); w > 59 {
			t.Fatalf("row %q is %d cells", l, w)
		}
	}
	// With one row left, a long item shows only its first, truncated row.
	if got := s.appendLines(nil, 3, 60, plain); len(got) != 3 || !strings.Contains(got[2], "epsilon z…") {
		t.Fatalf("clipped rows %q", got)
	}
}

func TestSuggestScrollsWrappedItems(t *testing.T) {
	long := strings.Repeat("word ", 30)
	var items []Suggestion
	for i := range 8 {
		items = append(items, Suggestion{Label: string(rune('a' + i)), Description: long})
	}
	s := suggestState{items: items}
	plain := func(string) string { return "" }
	s.sel = 7
	got := s.appendLines(nil, 10, 60, plain)
	// Five two-row items fit; the selection is the last of them.
	if len(got) != 10 || s.off != 3 || !strings.HasPrefix(got[8], "› h") {
		t.Fatalf("off=%d rows %q", s.off, got)
	}
	s.move(1) // wraps to the first item
	if s.appendLines(nil, 10, 60, plain); s.off != 0 {
		t.Fatalf("off after wrap = %d", s.off)
	}
}
