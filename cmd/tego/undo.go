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
			a.dim("/undo is off for this session: %s", why)
		}
		return
	}
	_ = a.undo.Add(a.undoKey(), undo.Entry{Tree: tree, Prompt: session.Oneline(prompt, 70), Time: time.Now()})
}

// dim shows a line for the user: dim in the terminal, a notice in an
// editor (tego acp).
func (a *app) dim(format string, args ...any) {
	if a.notice != nil {
		a.notice(fmt.Sprintf(format, args...))
		return
	}
	a.ui.Dim(format, args...)
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

// label names the moment a choice goes back to.
func (c undoChoice) label() string {
	if c.e.Prompt == "" {
		return "before the /undo at " + c.e.Time.Format("15:04")
	}
	return "before: " + c.e.Prompt
}

// when names it after "before": the prompt "fix the test".
func (c undoChoice) when() string {
	if c.e.Prompt == "" {
		return "the /undo at " + c.e.Time.Format("15:04")
	}
	return fmt.Sprintf("the prompt %q", c.e.Prompt)
}

// undoChoices are the session's snapshots that restoring would change
// something for, newest first, and the snapshot of the files now (cur).
// A message instead says why there are none.
func (a *app) undoChoices(ctx context.Context) (cur string, choices []undoChoice, msg string, err error) {
	entries := a.undo.Entries(a.undoKey())
	if len(entries) == 0 {
		return "", nil, "nothing to undo yet: tego snapshots the files before each prompt", nil
	}
	if cur, err = a.undo.Snapshot(ctx); err != nil {
		return "", nil, "", fmt.Errorf("snapshot: %w", err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		restore, remove, err := a.undo.Changes(ctx, e.Tree, cur)
		if err != nil || len(restore)+len(remove) == 0 {
			continue // nothing would change
		}
		choices = append(choices, undoChoice{e, restore, remove})
	}
	if len(choices) == 0 {
		return cur, nil, "nothing to undo: no file changed since the first prompt of this session", nil
	}
	return cur, choices, "", nil
}

// restoreUndo puts the files back as they were at c, keeping cur so /undo
// can undo itself, and tells the agent. It returns what it did.
func (a *app) restoreUndo(ctx context.Context, cur string, c undoChoice) (string, error) {
	_ = a.undo.Add(a.undoKey(), undo.Entry{Tree: cur, Time: time.Now()})
	restored, removed, err := a.undo.Restore(ctx, c.e.Tree, cur)
	if err != nil {
		return "", err
	}
	a.ag.Notice(fmt.Sprintf("The user used /undo: the project files are back to how they were before %s. "+
		"Every file change since then is undone, yours and any by commands (%s). Read files again before editing them.",
		c.when(), quoteFiles(append(restored, removed...), 8)))
	return fileCounts(restored, removed), nil
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
	ctx := context.Background()
	cur, choices, msg, err := a.undoChoices(ctx)
	switch {
	case err != nil:
		a.ui.Errorf("%v", err)
		return
	case msg != "":
		a.ui.Dim("%s", msg)
		return
	}
	items := make([]lineedit.PickItem, len(choices))
	for i, c := range choices {
		items[i] = lineedit.PickItem{Label: c.label(), Detail: ago(c.e.Time) + " · " + fileCounts(c.restore, c.remove)}
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
	done, err := a.restoreUndo(ctx, cur, c)
	if err != nil {
		a.ui.Errorf("undo: %v", err)
		return
	}
	a.ui.Dim("%s; /undo again to put them back", done)
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
