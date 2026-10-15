package markdown

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// spans renders s's inline items as "[flags@url:text]" runs.
func spans(s string) string {
	items := parseInline(nil, s, 0, "")
	var b strings.Builder
	var cur style
	curURL, open := "", false
	for _, it := range items {
		t := it.text
		if it.dch != 0 {
			t = t[:it.n]
		}
		if t == "" {
			continue
		}
		if !open || it.st != cur || it.url != curURL {
			if open && (cur != 0 || curURL != "") {
				b.WriteByte(']')
			}
			cur, curURL, open = it.st, it.url, true
			if cur != 0 || curURL != "" {
				b.WriteByte('[')
				for i, f := range "BISUDC" {
					if cur&(1<<i) != 0 {
						b.WriteRune(f)
					}
				}
				if curURL != "" {
					b.WriteString("@" + curURL)
				}
				b.WriteByte(':')
			}
		}
		b.WriteString(t)
	}
	if open && (cur != 0 || curURL != "") {
		b.WriteByte(']')
	}
	return b.String()
}

func TestInline(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"**bold** and *it*", "[B:bold] and [I:it]"},
		{"***both***", "[BI:both]"},
		{"**bold *nested* bold**", "[B:bold ][BI:nested][B: bold]"},
		{"*a **b** c*", "[I:a ][BI:b][I: c]"},
		{"`code **x**` y", "[C:code **x**] y"},
		{"``a`b`` c", "[C:a`b] c"},
		{"` a ` b", "[C:a] b"},
		{"`a` **`b`**", "[C:a] [BC:b]"},
		{"snake_case_name and _it_", "snake_case_name and [I:it]"},
		{"a_b_ c", "a_b_ c"},
		{"foo*bar*baz", "foo[I:bar]baz"},
		{"2 * 3 * 4", "2 * 3 * 4"},
		{"**unclosed", "**unclosed"},
		{"`unclosed", "`unclosed"},
		{`\*not\* italic \_x\_`, "*not* italic _x_"},
		{"~~gone~~ ~x~ ~~~no~~~", "[S:gone] [S:x] ~~~no~~~"},
		{"[a **b**](http://x/a_b) z", "[U@http://x/a_b:a ][BU@http://x/a_b:b][D: (http://x/a_b)] z"},
		{`[t](u "title")`, "[U@u:t][D: (u)]"},
		{"**[t](u)**", "[BU@u:t][BD: (u)]"},
		{"[https://e.com](https://e.com)", "[U@https://e.com:https://e.com]"},
		{"[^1] and [x] and [y]z", "[^1] and [x] and [y]z"},
		{"see https://e.com/a_b_c*d* ok", "see https://e.com/a_b_c*d* ok"},
		{"**https://e.com/x_y**", "[B:https://e.com/x_y]"},
		{"<https://x.y/*a*> *b*", "<https://x.y/*a*> [I:b]"},
		{"[a `]` b](u)", "[U@u:a ][UC@u:]][U@u: b][D: (u)]"},
	} {
		if got := spans(c.in); got != c.want {
			t.Errorf("%q:\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

func TestTable(t *testing.T) {
	doc := "| a | b | c |\n|:--|:-:|--:|\n| x | yy | zzz |\n| long | b \\| d | c |\n"
	want := "" +
		"┌──────┬───────┬─────┐\n" +
		"│ a    │   b   │   c │\n" +
		"├──────┼───────┼─────┤\n" +
		"│ x    │  yy   │ zzz │\n" +
		"├──────┼───────┼─────┤\n" +
		"│ long │ b | d │   c │\n" +
		"└──────┴───────┴─────┘\n"
	if got := Render(doc, 80, false); got != want {
		t.Errorf("box:\n%s\nwant:\n%s", got, want)
	}
	// Too narrow for the longest words: they are broken.
	want = "" +
		"┌─────┬───┬───┐\n" +
		"│ a   │ b │ c │\n" +
		"├─────┼───┼───┤\n" +
		"│ x   │ y │ z │\n" +
		"│     │ y │ z │\n" +
		"│     │   │ z │\n" +
		"├─────┼───┼───┤\n" +
		"│ lon │ b │ c │\n" +
		"│ g   │ | │   │\n" +
		"│     │ d │   │\n" +
		"└─────┴───┴───┘\n"
	if got := Render(doc, 15, false); got != want {
		t.Errorf("narrow:\n%s\nwant:\n%s", got, want)
	}
	// Less than one cell per column: printed raw.
	if got, want := Render(doc, 12, false), doc; got != want {
		t.Errorf("raw:\n%s\nwant:\n%s", got, want)
	}
	styled := "| **bold** | `c` |\n|---|---|\n| x | [l](u) |\n"
	if got, want := stripANSI(Render(styled, 80, true)), Render(styled, 80, false); got != want {
		t.Errorf("styled widths:\n%s\nwant:\n%s", got, want)
	}
	// Not a table: the candidate header falls back to a paragraph.
	if got, want := Render("a | b\nc\n", 80, false), "a | b\nc\n"; got != want {
		t.Errorf("candidate: %q, want %q", got, want)
	}
}

func TestTableWrap(t *testing.T) {
	doc := "| | tego | omp |\n|---|---|--:|\n" +
		"| Bahasa | Go, sekitar 19,5 rb baris | TypeScript/Bun, dikompilasi menjadi binary |\n" +
		"| Test | Ada di 7 paket (llm, mcp, lineedit, markdown). `agent` belum punya test. | - |\n"
	want := "" +
		"┌────────┬──────────────────────────┬──────────────────────┐\n" +
		"│        │ tego                     │                  omp │\n" +
		"├────────┼──────────────────────────┼──────────────────────┤\n" +
		"│ Bahasa │ Go, sekitar 19,5 rb      │      TypeScript/Bun, │\n" +
		"│        │ baris                    │  dikompilasi menjadi │\n" +
		"│        │                          │               binary │\n" +
		"├────────┼──────────────────────────┼──────────────────────┤\n" +
		"│ Test   │ Ada di 7 paket (llm,     │                    - │\n" +
		"│        │ mcp, lineedit,           │                      │\n" +
		"│        │ markdown). agent belum   │                      │\n" +
		"│        │ punya test.              │                      │\n" +
		"└────────┴──────────────────────────┴──────────────────────┘\n"
	if got := Render(doc, 60, false); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := stripANSI(Render(doc, 60, true)); got != want {
		t.Errorf("color layout:\n%s\nwant:\n%s", got, want)
	}
	if got, want := Render("| a<br>b | c<br/>d<BR />e |\n|---|---|\n", 80, false),
		"┌───┬───┐\n│ a │ c │\n│ b │ d │\n│   │ e │\n├───┼───┤\n└───┴───┘\n"; got != want {
		t.Errorf("<br>:\n%s\nwant:\n%s", got, want)
	}
}

// Styled text wrapped across lines is re-opened on each line and reset at
// its end.
func TestTableWrapStyles(t *testing.T) {
	doc := "| h |\n|---|\n| x **bold words that wrap** [a link](u) y |\n"
	got := Render(doc, 14, true)
	for _, want := range []string{
		"\x1b[1mbold\x1b[0m", "\x1b[1mwords that\x1b[0m", "\x1b[1mwrap\x1b[0m",
		"\x1b]8;;u\x1b\\\x1b[4ma\x1b]8;;\x1b\\\x1b[0m", "\x1b]8;;u\x1b\\\x1b[4mlink\x1b]8;;\x1b\\\x1b[0m",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, l := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if w := runewidth.StringWidth(stripANSI(l)); w != 14 {
			t.Errorf("line width %d: %q", w, l)
		}
		open, link := false, false
		for i := 0; i < len(l); {
			if l[i] != 0x1b {
				i++
				continue
			}
			j := skipEscape([]byte(l), i)
			switch seq := l[i:j]; {
			case seq == sgrReset:
				open = false
			case strings.HasPrefix(seq, "\x1b["):
				open = true
			case seq == osc8Close:
				link = false
			default:
				link = true
			}
			i = j
		}
		if open || link {
			t.Errorf("unbalanced line %q", l)
		}
	}
}

func TestHeadings(t *testing.T) {
	got := Render("# One\n## Two ##\n### Three\n#### *Four*\n", 80, true)
	want := "\x1b[1;4;38;5;215mOne\x1b[0m\n\x1b[1;38;5;215mTwo\x1b[0m\n" +
		"\x1b[1;38;5;215m### Three\x1b[0m\n\x1b[1;38;5;215m#### \x1b[0m\x1b[1;3;38;5;215mFour\x1b[0m\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestBlocks(t *testing.T) {
	doc := "## Head ##\n- a\n  - b\n    - c\n- [x] d\n3. e\n> *q*\n> - f\n\n***\n\n    indented *code*\n~~~\n*x*\n~~~\n"
	want := "Head\n• a\n  ◦ b\n    ▪ c\n☑ d\n3. e\n│ q\n│ • f\n\n" + strings.Repeat("─", 30) + "\n\n    indented *code*\n~~~\n*x*\n~~~\n"
	if got := Render(doc, 30, false); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got, want := stripANSI(Render(doc, 30, true)), want; got != want {
		t.Errorf("color layout:\n%s\nwant:\n%s", got, want)
	}
}

func TestHighlight(t *testing.T) {
	got := Render("```go\ns := `a\nb` // c\n/* x\ny */ n := 0x1F\n```\n", 80, true)
	for _, want := range []string{
		hlString + "`a" + sgrReset, hlString + "b`" + sgrReset, hlComment + "// c" + sgrReset,
		hlComment + "/* x" + sgrReset, hlComment + "y */" + sgrReset, hlNumber + "0x1F" + sgrReset,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// screen emulates the terminal behaviour Stream relies on.
type screen struct {
	w    int
	rows [][]rune
	r, c int
}

func (s *screen) write(t *testing.T, out string) {
	for i := 0; i < len(out); {
		if out[i] == 0x1b {
			j := skipEscape([]byte(out), i)
			seq := out[i:j]
			i = j
			switch {
			case strings.HasPrefix(seq, "\x1b]") || strings.HasSuffix(seq, "m"):
			case strings.HasSuffix(seq, "A"):
				n, _ := strconv.Atoi(seq[2 : len(seq)-1])
				s.r = max(0, s.r-max(n, 1))
				s.c = min(s.c, s.w-1)
			case seq == "\x1b[J":
				s.grow()
				if s.c < len(s.rows[s.r]) {
					s.rows[s.r] = s.rows[s.r][:s.c]
				}
				s.rows = s.rows[:s.r+1]
			default:
				t.Fatalf("unexpected escape %q", seq)
			}
			continue
		}
		r, n := []rune(out[i:])[0], len(string([]rune(out[i:])[0]))
		i += n
		switch r {
		case '\r':
			s.c = 0
		case '\n':
			s.r, s.c = s.r+1, 0
		default:
			rw := runewidth.RuneWidth(r)
			if s.c+rw > s.w {
				s.r, s.c = s.r+1, 0
			}
			s.grow()
			for len(s.rows[s.r]) < s.c+rw {
				s.rows[s.r] = append(s.rows[s.r], ' ')
			}
			s.rows[s.r][s.c] = r
			s.c += rw
		}
	}
}

func (s *screen) grow() {
	for len(s.rows) <= s.r {
		s.rows = append(s.rows, nil)
	}
}

func (s *screen) String() string {
	var b strings.Builder
	for _, r := range s.rows {
		b.WriteString(strings.TrimRight(string(r), " "))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

type recorder struct{ strings.Builder }

func (r *recorder) Write(p []byte) (int, error) { return r.Builder.Write(p) }

func stripANSI(s string) string {
	b := []byte(s)
	var out []byte
	for i := 0; i < len(b); {
		if b[i] == 0x1b {
			i = skipEscape(b, i)
			continue
		}
		out = append(out, b[i])
		i++
	}
	return string(out)
}

var sample = "# Title\n\nSome *intro* text with `code` and a [link](https://example.com).\n" +
	"This is a rather long line that must wrap across several rows of the narrow test screen, ok.\n\n" +
	"- item one\n  - nested **two**\n- [ ] task\n- [x] done\n\n1. first\n2. second\n\n" +
	"> quoted *text*\n\n---\n\n" +
	"```go\nfunc main() { fmt.Println(\"hi\") } // c\n```\n\n" +
	"| Name | Value |\n|:-----|------:|\n| a    | 1     |\n| b    | 22    |\n" +
	"After table, with a | pipe.\n\n" +
	"```mermaid\ngraph TD\nA-->B\n```\n\n```mermaid\nfail\n```\n\n" +
	"| x |\n|---|\n\n" +
	strings.Repeat("w", 40) + "\n" + strings.Repeat("v", 80) + "\n" +
	"| " + strings.Repeat("c", 36) + " |\n|---|\n| " + strings.Repeat("d", 76) + " |\n\n" +
	"trailing line without newline"

func TestStreamEquivalence(t *testing.T) {
	const width = 40
	docs := map[string]string{"lf": sample, "crlf": strings.ReplaceAll(sample, "\n", "\r\n")}
	for name, doc := range docs {
		for _, color := range []bool{false, true} {
			var want screen
			want.w = width
			want.write(t, Render(doc, width, color)+Render(doc, width, color))
			for _, size := range []int{len(doc), 1, 2, 3, 7, 64} {
				var rec recorder
				s := NewStream(&rec, Options{Width: func() int { return width }, Color: color})
				s.height = func() int { return 1000 }
				for range 2 {
					for i := 0; i < len(doc); i += size {
						s.Write(doc[i:min(i+size, len(doc))])
					}
					s.Close()
				}
				var got screen
				got.w = width
				got.write(t, rec.String())
				if got.String() != want.String() {
					t.Errorf("%s color=%v size=%d:\n%s\nwant:\n%s", name, color, size, got.String(), want.String())
				}
			}
		}
	}
}

func TestStreamFenceSplit(t *testing.T) {
	var rec recorder
	s := NewStream(&rec, Options{Color: true})
	for _, d := range []string{"```g", "o\r", "\nx := \"a // b\" // c\r\n``", "`\r", "\n*after* ``", " a `", "`\r", "\n"} {
		s.Write(d)
	}
	s.Close()
	var sc screen
	sc.w = 80
	sc.write(t, rec.String())
	if got, want := sc.String(), "```go\nx := \"a // b\" // c\n```\nafter a"; got != want {
		t.Errorf("screen %q, want %q", got, want)
	}
	out := rec.String()
	if !strings.Contains(out, hlComment+"// c") || !strings.Contains(out, "\x1b[3mafter") || strings.Contains(out, "\r\r") {
		t.Errorf("unexpected styling: %q", out)
	}
}

func TestStreamUnknownHeight(t *testing.T) {
	var rec recorder
	s := NewStream(&rec, Options{})
	s.Write("| a | b |\n|---|---|\n| 1 | 2 |\n\nx")
	s.Close()
	var sc screen
	sc.w = 80
	sc.write(t, rec.String())
	want := "| a | b |\n|---|---|\n| 1 | 2 |\n┌───┬───┐\n│ a │ b │\n├───┼───┤\n│ 1 │ 2 │\n└───┴───┘\n\nx"
	if got := sc.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
