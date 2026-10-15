package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/session"
	"github.com/akbxr/tego/internal/undo"
)

// snapshotFiles records the files before a prompt runs, for /undo.
func (a *app) snapshotFiles(ctx context.Context, prompt string) {
	if a.undo == nil || a.undo.Off() != "" {
		return
	}
	tree, err := a.undo.Snapshot(ctx)
	if err != nil {
		if why := a.undo.Off(); why != "" {
			a.ui.Dim("/undo is off for this session: %s", why)
		}
		return
	}
	_ = a.undo.Add(a.undoKey(), undo.Entry{Tree: tree, Prompt: session.Oneline(prompt, 70), Time: time.Now()})
}

// undoKey names this session's snapshots.
func (a *app) undoKey() string {
	if s := a.ag.Session; s != nil {
		return s.ID
	}
	return fmt.Sprintf("nosession-%d", os.Getpid())
}

type undoChoice struct {
	e               undo.Entry
	restore, remove []string
}

// undoFiles is /undo: pick a prompt of this session and put the files back
// as they were before it. Edits the user made since are undone too, which
// the confirmation says. The state just before is kept, so /undo can undo
// itself.
func (a *app) undoFiles() {
	switch {
	case a.cancel != nil:
		a.ui.Errorf("wait for the running turn to finish, or stop it, before /undo")
		return
	case a.undo == nil:
		a.ui.Errorf("/undo works in interactive sessions")
		return
	case a.undo.Off() != "":
		a.ui.Errorf("/undo is off: %s", a.undo.Off())
		return
	}
	entries := a.undo.Entries(a.undoKey())
	if len(entries) == 0 {
		a.ui.Dim("nothing to undo yet: tego snapshots the files before each prompt")
		return
	}
	ctx := context.Background()
	cur, err := a.undo.Snapshot(ctx)
	if err != nil {
		a.ui.Errorf("snapshot: %v", err)
		return
	}
	var choices []undoChoice
	var items []lineedit.PickItem
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		restore, remove, err := a.undo.Changes(ctx, e.Tree, cur)
		if err != nil || len(restore)+len(remove) == 0 {
			continue // nothing would change
		}
		label := "before: " + e.Prompt
		if e.Prompt == "" {
			label = "before the /undo at " + e.Time.Format("15:04")
		}
		choices = append(choices, undoChoice{e, restore, remove})
		items = append(items, lineedit.PickItem{Label: label, Detail: ago(e.Time) + " · " + fileCounts(restore, remove)})
	}
	if len(choices) == 0 {
		a.ui.Dim("nothing to undo: no file changed since the first prompt of this session")
		return
	}
	idx, err := a.tui.Pick("Undo file changes: put the files back as they were", items, "", 0)
	if err != nil {
		return
	}
	c := choices[idx]
	files := append(append([]string{}, c.restore...), c.remove...)
	ok, err := a.tui.Pick(fmt.Sprintf("%s? Changes you made yourself to these files since then are undone too.", strings.ToUpper(fileCounts(c.restore, c.remove)[:1])+fileCounts(c.restore, c.remove)[1:]),
		[]lineedit.PickItem{{Label: "Restore", Detail: quoteFiles(files, 4)}, {Label: "Cancel"}}, "", 0)
	if err != nil || ok != 0 {
		return
	}
	_ = a.undo.Add(a.undoKey(), undo.Entry{Tree: cur, Time: time.Now()})
	restored, removed, err := a.undo.Restore(ctx, c.e.Tree, cur)
	if err != nil {
		a.ui.Errorf("undo: %v", err)
		return
	}
	a.ui.Dim("%s; /undo again to put them back", fileCounts(restored, removed))
	when := "the /undo at " + c.e.Time.Format("15:04")
	if c.e.Prompt != "" {
		when = fmt.Sprintf("the prompt %q", c.e.Prompt)
	}
	a.ag.Notice(fmt.Sprintf("The user used /undo: the project files are back to how they were before %s. "+
		"Every file change since then is undone, yours and any by commands (%s). Read files again before editing them.",
		when, quoteFiles(append(restored, removed...), 8)))
}

// fileCounts says what a restore does: "3 files back, 1 removed".
func fileCounts(restore, remove []string) string {
	var parts []string
	if n := len(restore); n > 0 {
		parts = append(parts, plural(n, "file")+" back")
	}
	if n := len(remove); n > 0 {
		parts = append(parts, plural(n, "new file")+" removed")
	}
	return strings.Join(parts, ", ")
}

func quoteFiles(files []string, max int) string {
	shown := files
	if len(shown) > max {
		shown = shown[:max]
	}
	s := strings.Join(shown, ", ")
	if n := len(files) - len(shown); n > 0 {
		s += fmt.Sprintf(" and %d more", n)
	}
	return s
}
