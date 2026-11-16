package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/undo"
)

// The snapshot before a prompt runs beside it: snapshotFiles returns at
// once, the snapshots are listed in prompt order, and /undo waits for the
// one in progress.
func TestSnapshotBesidePrompt(t *testing.T) {
	base, work := t.TempDir(), t.TempDir()
	file := filepath.Join(work, "a.txt")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{undo: undo.Open(base, work), ag: &agent.Agent{}, ui: newPrintUI()}
	a.snapshotFiles(context.Background(), "first")
	a.snapshotted()
	if err := os.WriteFile(file, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.snapshotFiles(ctx, "second")
	cancel() // the turn stopped at once: the snapshot is still taken
	cur, choices, msg, err := a.undoChoices(context.Background())
	if err != nil || msg != "" || cur == "" {
		t.Fatalf("%v %q", err, msg)
	}
	entries := a.undo.Entries(a.undoKey())
	if len(entries) != 2 || entries[0].Prompt != "first" || entries[1].Prompt != "second" {
		t.Fatalf("entries %+v", entries)
	}
	if len(choices) != 1 || choices[0].e.Prompt != "first" {
		t.Fatalf("choices %+v", choices)
	}
}
