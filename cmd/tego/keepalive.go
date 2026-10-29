package main

import (
	"context"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// The REPL's side of the cache keep-alive (agent/keepalive.go): while no
// turn runs it asks the agent each second whether one is due, sends it off
// the REPL's goroutine, and hands the outcome back on it. A turn starts
// only once the one in flight has ended, so the agent sees whether its
// cache is warm.

// keepAliveTimeout bounds a keep-alive: a prompt submitted meanwhile waits
// for it.
const keepAliveTimeout = 20 * time.Second

type keepAliveResult struct {
	req   *llm.Request
	start time.Time
	usage llm.Usage
	err   error
}

// keepAliveTick sends a keep-alive if one is due and none is in flight.
func (a *app) keepAliveTick() {
	if a.cancel != nil || a.keepAliveBusy || a.ag == nil {
		return
	}
	req := a.ag.KeepAliveDue()
	if req == nil {
		return
	}
	p := a.ag.Provider
	a.keepAliveBusy = true
	start := time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), keepAliveTimeout)
		defer cancel()
		u, err := llm.Prewarm(ctx, p, req)
		a.keepAliveCh <- keepAliveResult{req, start, u, err}
	}()
}

func (a *app) keepAliveFinish(r keepAliveResult) {
	a.keepAliveBusy = false
	a.ag.KeepAliveDone(r.req, r.start, r.usage, r.err)
}

// keepAliveWait lets the keep-alive in flight end before a turn starts.
func (a *app) keepAliveWait() {
	if a.keepAliveBusy {
		a.keepAliveFinish(<-a.keepAliveCh)
	}
}
