package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// feed sends s in chunks of n bytes, as a provider streams arguments.
func feed(s string, n int) *partialArgs {
	p := newPartialArgs()
	for len(s) > 0 {
		k := min(n, len(s))
		p.add(s[:k])
		s = s[k:]
	}
	return p
}

func TestPartialArgsWrite(t *testing.T) {
	content := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"héllo 👋 \\\"q\\\"\")\n}\n"
	c, _ := json.Marshal(content)
	raw := []byte(`{"path":"cmd/main.go","content":` + string(c) + `}`) // path first, as models write it
	for _, n := range []int{1, 3, 7, 64, len(raw)} {                    // every chunk size, cuts inside escapes and runes included
		p := feed(string(raw), n)
		if p.value("path") != "cmd/main.go" || p.value("content") != content {
			t.Fatalf("chunk %d: path %q content %q", n, p.value("path"), p.value("content"))
		}
	}
	// Mid-stream: the value so far, and the path once it's complete.
	cut := string(raw)[:strings.Index(string(raw), "fmt.Println")]
	p := feed(cut, 5)
	if p.value("path") != "cmd/main.go" || !strings.HasSuffix(p.value("content"), "func main() {\n\t") {
		t.Fatalf("partial: %q", p.value("content"))
	}
	// The last 2 lines, a blank one inside kept; the line just begun ("\t")
	// is neither shown nor counted.
	lines, total := p.tail("content", false, 2)
	if total != 5 || len(lines) != 2 || lines[0] != "" || lines[1] != "func main() {" {
		t.Fatalf("tail %q of %d", lines, total)
	}
}

func TestPartialArgsEdits(t *testing.T) {
	raw := `{"path":"a.go","edits":[{"old_string":"x := 1","new_string":"x := 2\ny := 3"},{"old_string":"z","new_string":"w\nv"}],"replace_all":false}`
	p := feed(raw, 4)
	if p.value("path") != "a.go" || len(p.items) != 2 {
		t.Fatalf("path %q items %d", p.value("path"), len(p.items))
	}
	if p.itemValue(0, "new_string") != "x := 2\ny := 3" || p.itemValue(1, "old_string") != "z" || p.itemValue(1, "new_string") != "w\nv" {
		t.Fatalf("items %v", p.items)
	}
	// Half-way through the second change.
	p = feed(raw[:strings.Index(raw, `"w\nv"`)+4], 4) // up to and including the \n escape
	if len(p.items) != 2 || p.itemValue(1, "new_string") != "w\n" {
		t.Fatalf("partial item %q", p.itemValue(1, "new_string"))
	}
	if lines, _ := p.tail("new_string", true, 3); len(lines) != 1 || lines[0] != "w" {
		t.Fatalf("item tail %q", lines)
	}
	// A key inside an edits item never reads as a top-level field.
	if p.value("new_string") != "" {
		t.Fatalf("leaked %q", p.value("new_string"))
	}
}

func TestPartialArgsSingleEditAndUnicodeEscapes(t *testing.T) {
	raw := `{"path":"b.py","old_string":"a","new_string":"caf\u00e9 \ud83d\ude00\ttab"}`
	p := feed(raw, 2)
	if p.value("new_string") != "café 😀\ttab" {
		t.Fatalf("new_string %q", p.value("new_string"))
	}
}
