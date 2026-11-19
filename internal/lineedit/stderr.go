package lineedit

import (
	"os"
	"runtime/debug"

	"golang.org/x/sys/unix"
)

// stderrGuard points fd 2 at /dev/null while the TUI owns the terminal.
// Bytes written straight to fd 2 skip the renderer and land where the
// cursor is, in the editor. macOS's malloc prints "MallocStackLogging:
// can't turn off malloc stack logging…" from inside fork, in the child
// before it execs, so a long, busy session may get one such line per
// command it runs. Each moves the screen under the footer, and the next
// redraws leave pieces of the old footer in the scrollback. A crash still
// shows: while the guard is on, the runtime prints it to the terminal
// through SetCrashOutput.
type stderrGuard struct {
	tty  *os.File // what fd 2 was
	null int
}

// newStderrGuard returns nil when it can't set up, and the nil guard does
// nothing.
func newStderrGuard() *stderrGuard {
	saved, err := unix.Dup(2)
	if err != nil {
		return nil
	}
	unix.CloseOnExec(saved)
	null, err := unix.Open(os.DevNull, unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(saved)
		return nil
	}
	return &stderrGuard{tty: os.NewFile(uintptr(saved), "/dev/stderr"), null: null}
}

// on sends fd 2 to /dev/null, and crashes to the terminal.
func (g *stderrGuard) on() {
	if g == nil {
		return
	}
	_ = dup2(g.null, 2)
	_ = debug.SetCrashOutput(g.tty, debug.CrashOptions{})
}

// off points fd 2 back at the terminal, for a child that uses it or for
// tego's own messages once the TUI is gone.
func (g *stderrGuard) off() {
	if g == nil {
		return
	}
	_ = dup2(int(g.tty.Fd()), 2)
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
}

func (g *stderrGuard) close() {
	if g == nil {
		return
	}
	g.off()
	_ = g.tty.Close()
	_ = unix.Close(g.null)
}
