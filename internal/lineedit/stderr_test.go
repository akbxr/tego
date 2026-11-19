package lineedit

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// While the guard is on, bytes written straight to fd 2 (by malloc in a
// forked child, say) miss the terminal; off and close give it back.
func TestStderrGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tty")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	orig, err := unix.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = dup2(orig, 2)
		unix.Close(orig)
	}()
	if err := dup2(int(f.Fd()), 2); err != nil {
		t.Fatal(err)
	}
	write := func(s string) { _, _ = unix.Write(2, []byte(s)) }

	g := newStderrGuard()
	if g == nil {
		t.Fatal("no guard")
	}
	g.on()
	write("hidden ")
	child := exec.Command("sh", "-c", "printf 'child-hidden ' >&2")
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	g.off()
	write("shown ")
	child = exec.Command("sh", "-c", "printf 'child-shown ' >&2")
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	g.on()
	write("hidden-again ")
	g.close()
	write("after")
	(*stderrGuard)(nil).on() // a TUI without a guard

	got, _ := os.ReadFile(path)
	if string(got) != "shown child-shown after" {
		t.Fatalf("fd 2 got %q", got)
	}
}
