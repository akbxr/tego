package tools

import "testing"

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "a/b/main.go", true}, // no slash: base name at any depth
		{"*.go", "main.gox", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"src/**/*.ts", "src/x.ts", true},
		{"src/**/*.ts", "src/a/b/x.ts", true},
		{"src/**/*.ts", "lib/src/x.ts", false},
		{"src/*.ts", "src/a/x.ts", false},
		{"src/**", "src/a/b", true},
		{"./src/*.ts", "src/x.ts", true},
		{"*.{go,md}", "x/README.md", true},
		{"*.{go,md}", "x/a.txt", false},
		{"{cmd,internal}/**/*_test.go", "internal/tools/a_test.go", true},
		{"{cmd,internal}/**/*_test.go", "pkg/a_test.go", false},
		{"a/{b,c/{d,e}}/f", "a/c/e/f", true},
		{"a/{b,c/{d,e}}/f", "a/c/f", false},
		{"file?.txt", "file1.txt", true},
		{"file?.txt", "file10.txt", false},
		{"[abc].go", "b.go", true},
		{"[!abc].go", "b.go", false},
		{"[!abc].go", "d.go", true},
		{"**", "any/thing", true},
	}
	for _, c := range cases {
		m, err := compileGlob(c.pat)
		if err != nil {
			t.Fatalf("%s: %v", c.pat, err)
		}
		if got := m.Match(c.path); got != c.want {
			t.Errorf("match(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestGlobBadPattern(t *testing.T) {
	if _, err := compileGlob("[abc"); err == nil {
		t.Fatal("want error for unterminated class")
	}
}

func TestExpandBraces(t *testing.T) {
	got, err := expandBraces("x{a,b{1,2}}y{}")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"xay{}", "xb1y{}", "xb2y{}"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
