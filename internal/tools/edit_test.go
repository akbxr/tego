package tools

import (
	"strings"
	"testing"
)

func TestApplyEditExactUnique(t *testing.T) {
	res, err := applyEdit("a\nfoo\nb\n", "foo", "bar", false)
	if err != nil || res.content != "a\nbar\nb\n" || res.count != 1 || res.how != "" {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestApplyEditMultiple(t *testing.T) {
	_, err := applyEdit("x\nfoo\ny\nfoo\n", "foo", "bar", false)
	if err == nil || !strings.Contains(err.Error(), "lines 2, 4") {
		t.Fatalf("want multi-match error with lines, got %v", err)
	}
	res, err := applyEdit("x\nfoo\ny\nfoo\n", "foo", "bar", true)
	if err != nil || res.content != "x\nbar\ny\nbar\n" || res.count != 2 {
		t.Fatalf("replace_all: %+v %v", res, err)
	}
}

func TestApplyEditEmptyAndIdentical(t *testing.T) {
	if _, err := applyEdit("a", "", "b", false); err == nil {
		t.Fatal("empty old_string must fail")
	}
	if _, err := applyEdit("a", "a", "a", false); err == nil {
		t.Fatal("identical strings must fail")
	}
}

func TestApplyEditCRLFPreserved(t *testing.T) {
	content := "one\r\ntwo\r\nthree\r\n"
	res, err := applyEdit(content, "one\ntwo\n", "uno\ndos\nextra\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "uno\r\ndos\r\nextra\r\nthree\r\n"; res.content != want {
		t.Fatalf("got %q want %q", res.content, want)
	}
	if res.how == "" {
		t.Fatal("expected fallback marker")
	}
}

func TestApplyEditCRLFExactNewGetsCRLF(t *testing.T) {
	res, err := applyEdit("a\r\nb\r\n", "b", "b\nc", false)
	if err != nil || res.content != "a\r\nb\r\nc\r\n" {
		t.Fatalf("got %q %v", res.content, err)
	}
}

func TestApplyEditNoTrailingNewlinePreserved(t *testing.T) {
	res, err := applyEdit("a\nb", "b", "c", false)
	if err != nil || res.content != "a\nc" {
		t.Fatalf("got %q %v", res.content, err)
	}
}

func TestApplyEditTrailingWhitespace(t *testing.T) {
	content := "func f() {   \n\treturn 1\t\n}\n"
	res, err := applyEdit(content, "func f() {\n\treturn 1\n}", "func f() {\n\treturn 2\n}", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.content != "func f() {\n\treturn 2\n}\n" || res.how != "ignoring trailing whitespace" {
		t.Fatalf("got %q (%s)", res.content, res.how)
	}
}

func TestApplyEditIndentation(t *testing.T) {
	content := "class A:\n    def f(self):\n        return 1\n\n    def g(self):\n        pass\n"
	old := "def f(self):\n    return 1\n"
	newS := "def f(self):\n    x = 1\n    return x\n"
	res, err := applyEdit(content, old, newS, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "class A:\n    def f(self):\n        x = 1\n        return x\n\n    def g(self):\n        pass\n"
	if res.content != want || res.how != "ignoring indentation" {
		t.Fatalf("got %q (%s)", res.content, res.how)
	}
}

func TestApplyEditIndentationRemove(t *testing.T) {
	content := "if x {\n\tfoo()\n\tbar()\n}\n"
	res, err := applyEdit(content, "\t\tfoo()\n\t\tbar()", "\t\tfoo()\n\t\t\tbaz()\n\t\tbar()", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "if x {\n\tfoo()\n\t\tbaz()\n\tbar()\n}\n"; res.content != want {
		t.Fatalf("got %q", res.content)
	}
}

func TestApplyEditIndentationCRLF(t *testing.T) {
	content := "a\r\n  b\r\n  c\r\nd\r\n"
	res, err := applyEdit(content, "b\nc", "B\nC", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "a\r\n  B\r\n  C\r\nd\r\n"; res.content != want {
		t.Fatalf("got %q", res.content)
	}
}

func TestApplyEditFallbackMultiple(t *testing.T) {
	content := "  x()\n  y()\n\n    x()\n    y()\n"
	if _, err := applyEdit(content, "x()\ny()", "z()", false); err == nil || !strings.Contains(err.Error(), "lines 1, 4") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
}

func TestApplyEditNotFoundHint(t *testing.T) {
	content := "package main\n\nfunc main() {\n\tfmt.Println(\"hello world\")\n}\n"
	_, err := applyEdit(content, "\tfmt.Println(\"hello wrld\")", "x", false)
	if err == nil || !strings.Contains(err.Error(), "lines 4-4") || !strings.Contains(err.Error(), "4\t\tfmt.Println") {
		t.Fatalf("want hint pointing at line 4, got %v", err)
	}
}

func TestDiffSnippet(t *testing.T) {
	d := diffSnippet("a\nb\nc\nd\ne\n", "a\nb\nX\nY\nd\ne\n", 40)
	want := " 1\ta\n 2\tb\n-3\tc\n+3\tX\n+4\tY\n 5\td\n 6\te"
	if d != want {
		t.Fatalf("got\n%s\nwant\n%s", d, want)
	}
}
