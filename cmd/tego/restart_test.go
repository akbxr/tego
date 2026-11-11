package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/worktree"
)

// /restart starts the same session again as it is: its file, model,
// thinking, permissions, fast mode and worktree.
func TestRestartArgs(t *testing.T) {
	a := peerApp(t, watchingProvider{})
	a.ag.ProviderName, a.ag.Model, a.ag.Thinking, a.ag.Fast = "anthropic", "claude-fable-5-2", "", true
	a.ag.Perms = agent.NewPermissions(agent.PermEdits)
	if got := strings.Join(a.restartArgs(), " "); got != "-m anthropic/claude-fable-5-2 -t off --permissions edits --fast" {
		t.Fatalf("no messages yet: %s", got)
	}
	if err := a.ag.Session.Append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	a.ag.Messages = []llm.Message{{Role: llm.RoleUser}}
	a.ag.Thinking, a.ag.Fast = "high", false
	a.worktree = &worktree.Tree{Name: "fix", Path: "/repo-wt/fix", Repo: "/repo"}
	want := "-w fix -r " + a.ag.Session.Path + " -m anthropic/claude-fable-5-2 -t high --permissions edits"
	if got := strings.Join(a.restartArgs(), " "); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if !a.restartCommand() || a.restart == nil || a.restart.dir != "/repo" || !a.restart.keepWorktree {
		t.Fatalf("restart %+v", a.restart)
	}

	// A session not saved is not restarted.
	a.restart, a.noSession = nil, true
	if a.restartCommand() || a.restart != nil {
		t.Fatal("restarted a --no-session session")
	}
}

// A restart keeps its worktree as it is; ending the session finishes it.
func TestRestartKeepsTheWorktree(t *testing.T) {
	dir, home := gitRepo(t)
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	wt, _, _ := worktree.Open(home, dir, "empty")
	os.Chdir(wt.Path)
	(&app{worktree: wt, restart: &restartError{keepWorktree: true}}).finishWorktree()
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatal("the worktree went on a restart")
	}
	(&app{worktree: wt}).finishWorktree()
	if _, err := os.Stat(wt.Path); err == nil {
		t.Fatal("an empty worktree was kept at the end")
	}
}

// A binary replaced by another build is told about once; the same build
// installed again is not.
func TestStaleBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tego")
	install := func(v string) {
		t.Helper()
		tmp := filepath.Join(dir, "new")
		if err := os.WriteFile(tmp, []byte("#!/bin/sh\necho 'tego "+v+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, exe); err != nil { // as make install and tego update do
			t.Fatal(err)
		}
	}
	install(version)
	fi, _ := os.Stat(exe)
	w := &staleWatch{exe: exe, exeInfo: fi}
	if v, ok := w.replaced(); ok {
		t.Fatalf("unchanged: %q", v)
	}
	install(version)
	if v, ok := w.replaced(); ok {
		t.Fatalf("the same build again: %q", v)
	}
	install("0.9.9")
	if v, ok := w.replaced(); !ok || v != "0.9.9" {
		t.Fatalf("new build: %q %v", v, ok)
	}
	if _, ok := w.replaced(); ok {
		t.Fatal("told twice")
	}
}

// config.json changed by hand or by another session is told about, but not
// the keys a restart keeps this session's own of, nor this session's
// saves.
func TestConfigChanged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEGO_HOME", dir)
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"model":"a/b","subagent_max_turns":0}`), 0o600)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}
	if got := a.configChanged(); got != "" {
		t.Fatalf("unchanged: %q", got)
	}
	cfg.Thinking = "high"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if got := a.configChanged(); got != "" {
		t.Fatalf("its own save: %q", got)
	}
	data, _ := os.ReadFile(path)
	edited := strings.Replace(string(data), `"subagent_max_turns": 0`, `"subagent_max_turns": 0, "session_messages": true, "model": "c/d"`, 1)
	edited = strings.Replace(edited, `"model": "a/b",`, "", 1)
	os.WriteFile(path, []byte(edited), 0o600)
	if got := a.configChanged(); got != "session_messages" {
		t.Fatalf("edited:\n%s\n%q", edited, got)
	}
	os.WriteFile(path, []byte(`{"thinking":"high"}`), 0o600)
	if got := a.configChanged(); got != "subagent_max_turns" {
		t.Fatalf("a key gone: %q", got)
	}
}
