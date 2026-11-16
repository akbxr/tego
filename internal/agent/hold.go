package agent

import (
	"context"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// holdNote is how long a call waits on the hold before the user is told.
var holdNote = time.Second

// HoldTools makes calls that may change files wait until done is closed:
// the /undo snapshot of the files before the prompt, taken beside its
// first request instead of before it. Read-only calls don't wait. nil
// lifts it.
func (a *Agent) HoldTools(done <-chan struct{}) {
	a.holdMu.Lock()
	a.hold = done
	a.holdMu.Unlock()
}

// waitHold waits for the hold before call runs, unless it reads only.
func (a *Agent) waitHold(ctx context.Context, call llm.Block) {
	a.holdMu.Lock()
	done := a.hold
	a.holdMu.Unlock()
	if done == nil || a.readOnly(call) {
		return
	}
	select {
	case <-done:
		return
	default:
	}
	t := time.NewTimer(holdNote)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-ctx.Done():
		return
	case <-t.C:
		a.UI.OnInfo("waiting for the /undo snapshot of the files before " + call.Name + " runs")
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}
