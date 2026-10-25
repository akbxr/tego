package lsp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeSpec(mode string) Spec {
	return Spec{
		Name:      "fake",
		Command:   []string{os.Args[0]},
		Env:       []string{"TEGO_FAKE_LSP=" + mode},
		Languages: map[string]string{".fk": "fake"},
		Roots:     []string{"fake.mod"},
	}
}

// project makes a temp project with a root marker and returns its dir.
func project(t *testing.T) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.WriteFile(filepath.Join(dir, "fake.mod"), nil, 0o644)
	return dir
}

func rel(dir string) func(string) string {
	return func(p string) string {
		r, _ := filepath.Rel(dir, p)
		return r
	}
}

func TestCheckReportsErrorsOnly(t *testing.T) {
	dir := project(t)
	m := NewManager(dir, []Spec{fakeSpec("")})
	defer m.Close()
	f := filepath.Join(dir, "a.fk")

	note, ok := m.Check(context.Background(), f, "", "fine\nERR one\nWARN\n  ERR two", rel(dir))
	if !ok {
		t.Fatal("not checked")
	}
	want := "\n\nfake reports errors after this edit:\na.fk:2:3: bad: ERR one\na.fk:4:3: bad: ERR two"
	if note != want {
		t.Fatalf("note %q", note)
	}
	// Fixed: nothing to say, but it was checked.
	if note, ok := m.Check(context.Background(), f, "", "fine\nWARN", rel(dir)); !ok || note != "" {
		t.Fatalf("clean file: %q %v", note, ok)
	}
	if got := m.Running(); len(got) != 1 || !strings.HasPrefix(got[0], "fake (") {
		t.Fatalf("running %v", got)
	}
}

// Errors an edit causes in other files are reported, even at the first
// edit, but not ones they already had.
func TestCheckReportsNewErrorsInOtherFiles(t *testing.T) {
	ctx := context.Background()
	t.Run("first edit", func(t *testing.T) {
		dir := project(t)
		m := NewManager(dir, []Spec{fakeSpec("")})
		defer m.Close()
		f, other := filepath.Join(dir, "a.fk"), filepath.Join(dir, "b.fk")
		note, ok := m.Check(ctx, f, "ok", "BREAKS "+other, rel(dir))
		if !ok || note != "\n\nfake reports errors after this edit:\nb.fk:1:3: broken by the edit" {
			t.Fatalf("first edit: %q %v", note, ok)
		}
	})
	t.Run("already broken", func(t *testing.T) {
		dir := project(t)
		m := NewManager(dir, []Spec{fakeSpec("")})
		defer m.Close()
		f, other := filepath.Join(dir, "a.fk"), filepath.Join(dir, "b.fk")
		// b.fk was broken before tego touched a.fk: not this edit's doing.
		if note, _ := m.Check(ctx, f, "BREAKS "+other, "BREAKS "+other+"\nmore", rel(dir)); note != "" {
			t.Fatalf("an error another file had is blamed on the edit: %q", note)
		}
		m.Check(ctx, f, "", "ok", rel(dir)) // b.fk is clean again
		note, _ := m.Check(ctx, f, "", "x\nBREAKS "+other, rel(dir))
		if !strings.Contains(note, "b.fk:2:3: broken by the edit") {
			t.Fatalf("other file's new error missing: %q", note)
		}
		// Still broken, not newly: said once.
		if note, _ := m.Check(ctx, f, "", "x\nBREAKS "+other+"\n", rel(dir)); note != "" {
			t.Fatalf("old error in another file repeated: %q", note)
		}
	})
}

func TestCheckPullAndUnversioned(t *testing.T) {
	for _, mode := range []string{"pull", "noversion"} {
		t.Run(mode, func(t *testing.T) {
			dir := project(t)
			m := NewManager(dir, []Spec{fakeSpec(mode)})
			defer m.Close()
			f := filepath.Join(dir, "a.fk")
			for i, text := range []string{"ERR a", "ok", "z\nERR b"} {
				note, ok := m.Check(context.Background(), f, "", text, rel(dir))
				wantErr := strings.Contains(text, "ERR")
				if !ok || wantErr != strings.Contains(note, "bad: ERR") {
					t.Fatalf("check %d of %q: %q %v", i, text, note, ok)
				}
			}
		})
	}
}

// A server that cannot start is reported once and not tried again; the
// file is left to the syntax check.
func TestServerThatFailsIsNotRetried(t *testing.T) {
	dir := project(t)
	m := NewManager(dir, []Spec{fakeSpec("crash")})
	defer m.Close()
	var fails atomic.Int32
	var reason string
	m.OnFail = func(server, r string) { fails.Add(1); reason = server + ": " + r }
	f := filepath.Join(dir, "a.fk")
	for range 3 {
		if _, ok := m.Check(context.Background(), f, "", "ERR", rel(dir)); ok {
			t.Fatal("checked by a server that did not start")
		}
	}
	if fails.Load() != 1 || !strings.Contains(reason, "fake: cannot start") {
		t.Fatalf("fails %d, reason %q", fails.Load(), reason)
	}
}

// When a server fails to start the next one for the file is used, and the
// failure is not reported since a server runs. A program in the project's
// own bin directory is used before PATH.
func TestNextServerAndLocalBins(t *testing.T) {
	dir := project(t)
	bin := filepath.Join(dir, "node_modules", ".bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "fake-local"), []byte("#!/bin/sh\nexec "+os.Args[0]+" \"$@\"\n"), 0o755)
	crash := fakeSpec("crash")
	crash.Name = "first"
	local := fakeSpec("")
	local.Name, local.Command, local.LocalBins = "second", []string{"fake-local"}, []string{"node_modules/.bin"}
	m := NewManager(dir, []Spec{crash, local})
	defer m.Close()
	var fails []string
	m.OnFail = func(server, _ string) { fails = append(fails, server) }
	for range 2 {
		note, ok := m.Check(context.Background(), filepath.Join(dir, "a.fk"), "", "ERR", rel(dir))
		if !ok || !strings.HasPrefix(note, "\n\nsecond reports errors") {
			t.Fatalf("%q %v", note, ok)
		}
	}
	if len(fails) != 0 {
		t.Fatalf("reported %v though a server ran", fails)
	}
	// Not on PATH and not in the project: not a candidate.
	elsewhere := project(t)
	if c := NewManager(elsewhere, []Spec{local}).candidates(filepath.Join(elsewhere, "a.fk")); len(c) != 0 {
		t.Fatalf("candidates %+v", c)
	}
}

// Files no server checks, servers not installed and files outside a root a
// server needs are not checked.
func TestNotChecked(t *testing.T) {
	dir := project(t)
	missing := fakeSpec("")
	missing.Name, missing.Command, missing.Languages = "missing", []string{"tego-no-such-server"}, map[string]string{".zz": "zz"}
	needs := fakeSpec("")
	needs.Name, needs.Languages, needs.Roots, needs.NeedRoot = "needs", map[string]string{".nr": "nr"}, []string{"nothing.here"}, true
	m := NewManager(dir, []Spec{fakeSpec(""), missing, needs})
	defer m.Close()
	for _, f := range []string{"a.txt", "Makefile", "a.zz", "a.nr"} {
		if _, ok := m.Check(context.Background(), filepath.Join(dir, f), "", "ERR", nil); ok {
			t.Errorf("%s was checked", f)
		}
	}
	if got := m.Running(); len(got) != 0 {
		t.Fatalf("started %v", got)
	}
	var nilM *Manager
	if _, ok := nilM.Check(context.Background(), "a.fk", "", "", nil); ok {
		t.Fatal("nil manager checked")
	}
	nilM.Close()
}

// A server stops after sitting unused for Idle, and starts again at the
// next check; Close stops the rest.
func TestIdleStopAndClose(t *testing.T) {
	dir := project(t)
	m := NewManager(dir, []Spec{fakeSpec("")})
	m.Idle = 200 * time.Millisecond
	f := filepath.Join(dir, "a.fk")
	m.Check(context.Background(), f, "", "ok", nil)
	m.mu.Lock()
	var first *server
	for _, s := range m.servers {
		first = s
	}
	m.mu.Unlock()
	select {
	case <-first.done:
	case <-time.After(3 * time.Second):
		t.Fatal("idle server still running")
	}
	if note, ok := m.Check(context.Background(), f, "", "ERR", nil); !ok || note == "" {
		t.Fatalf("no check after an idle stop: %q %v", note, ok)
	}
	m.mu.Lock()
	var second *server
	for _, s := range m.servers {
		second = s
	}
	m.mu.Unlock()
	m.Close()
	select {
	case <-second.done:
	default:
		t.Fatal("server running after Close")
	}
	if _, ok := m.Check(context.Background(), f, "", "ERR", nil); ok {
		t.Fatal("checked after Close")
	}
}

func TestFindRoot(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	mk := func(p string) {
		os.MkdirAll(filepath.Dir(filepath.Join(base, p)), 0o755)
		os.WriteFile(filepath.Join(base, p), nil, 0o644)
	}
	mk("repo/go.mod")
	mk("repo/sub/go.mod")
	mk("repo/sub/pkg/x.go")
	mk("other/go.mod")
	mk("other/y.go")
	mk("lone/z.go")
	spec := Spec{Roots: []string{"go.mod"}}
	need := Spec{Roots: []string{"go.mod"}, NeedRoot: true}
	j := func(p string) string { return filepath.Join(base, p) }
	cases := []struct {
		spec      Spec
		file, cwd string
		want      string
	}{
		{spec, "repo/sub/pkg/x.go", "repo", "repo"},             // outermost inside cwd: one server per monorepo
		{spec, "repo/sub/pkg/x.go", "repo/sub/pkg", "repo/sub"}, // cwd below the module
		{spec, "other/y.go", "repo", "other"},                   // outside cwd: nearest
		{spec, "lone/z.go", "lone", "lone"},                     // no marker: cwd
		{need, "lone/z.go", "lone", ""},                         // ... unless one is needed
		{spec, "lone/z.go", "repo", "lone"},                     // a lone file elsewhere: its dir
	}
	for _, c := range cases {
		want := ""
		if c.want != "" {
			want = j(c.want)
		}
		if got := findRoot(c.spec, j(c.file), j(c.cwd)); got != want {
			t.Errorf("findRoot(%s, cwd %s) = %q, want %q", c.file, c.cwd, got, want)
		}
	}
}

func TestSpecsFromConfig(t *testing.T) {
	specs, err := Specs(map[string]json.RawMessage{
		"gopls":    []byte(`false`),
		"clangd":   []byte(`{"need_root": false, "env": {"B": "2", "A": "1"}}`),
		"luau-lsp": []byte(`{"command": ["luau-lsp", "lsp"], "languages": {"luau": "luau"}, "roots": [".luaurc"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	if _, ok := byName["gopls"]; ok {
		t.Error("gopls not turned off")
	}
	if c := byName["clangd"]; c.NeedRoot || c.Command[0] != "clangd" || len(c.Languages) == 0 || strings.Join(c.Env, ",") != "A=1,B=2" {
		t.Errorf("clangd changed wrong: %+v", c)
	}
	if l := byName["luau-lsp"]; l.Languages[".luau"] != "luau" || specs[0].Name != "clangd" && specs[0].Name != "luau-lsp" {
		t.Errorf("luau-lsp: %+v, first %s", l, specs[0].Name)
	}
	if _, err := Specs(map[string]json.RawMessage{"new": []byte(`{"command": ["x"]}`)}); err == nil {
		t.Error("a server without languages was accepted")
	}
	if _, err := Specs(map[string]json.RawMessage{"gopls": []byte(`"off"`)}); err == nil {
		t.Error("a bad entry was accepted")
	}
}

func TestURIs(t *testing.T) {
	p := "/tmp/a dir/ü#1.go"
	u := fileURI(p)
	if u != "file:///tmp/a%20dir/%C3%BC%231.go" || uriPath(u) != p {
		t.Fatalf("%q -> %q -> %q", p, u, uriPath(u))
	}
	if normURI("file:///tmp/a%20dir/%c3%bc%231.go") != u {
		t.Fatalf("normURI %q", normURI("file:///tmp/a%20dir/%c3%bc%231.go"))
	}
}

// With gopls installed: a type error, which a syntax check cannot see, is
// reported with its position, and a caller in another file broken by the
// edit is too.
func TestGopls(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil || testing.Short() {
		t.Skip("gopls not installed")
	}
	dir := project(t)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n\ngo 1.22\n"), 0o644)
	a, b := filepath.Join(dir, "a.go"), filepath.Join(dir, "b.go")
	os.WriteFile(a, []byte("package m\n\nfunc Add(x, y int) int { return x + y }\n"), 0o644)
	os.WriteFile(b, []byte("package m\n\nvar Sum = Add(1, 2)\n"), 0o644)
	m := NewManager(dir, Builtin)
	defer m.Close()
	ctx := context.Background()

	good := "package m\n\nfunc Add(x, y int) int { return x + y }\n"
	// The first edit: a caller in another file breaks.
	note, ok := m.Check(ctx, a, good, "package m\n\nfunc Plus(x, y int) int { return x + y }\n", rel(dir))
	if !ok || note != "\n\ngopls reports errors after this edit:\nb.go:3:11: undefined: Add" {
		t.Fatalf("caller in another file: %q %v", note, ok)
	}
	broken := "package m\n\nfunc Add(x, y int) int { return x + \"y\" }\n"
	note, ok = m.Check(ctx, a, "", broken, rel(dir))
	if !ok || !strings.HasPrefix(note, "\n\ngopls reports errors after this edit:\na.go:3:") || strings.Contains(note, "b.go") {
		t.Fatalf("type error: %q %v", note, ok)
	}
}
