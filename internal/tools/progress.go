package tools

import "context"

type progressKey struct{}

// WithProgress returns a context that makes long-running tools (bash) pass
// their output to fn as it arrives, besides returning it at the end. fn must
// be quick and must not keep the slice.
func WithProgress(ctx context.Context, fn func(chunk []byte)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func progressFrom(ctx context.Context) func([]byte) {
	fn, _ := ctx.Value(progressKey{}).(func([]byte))
	return fn
}

type callIDKey struct{}

// WithCallID returns a context carrying the id of the tool call a tool runs
// for, so a tool can tie what it shows (an editor's terminal) to the call.
func WithCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callIDKey{}, id)
}

// CallID is the id of the tool call ctx runs ("" if none).
func CallID(ctx context.Context) string {
	id, _ := ctx.Value(callIDKey{}).(string)
	return id
}

type progressWriter func([]byte)

func (f progressWriter) Write(p []byte) (int, error) {
	f(p)
	return len(p), nil
}

type videoKey struct{}

// WithVideo returns a context saying the model takes video files: tools
// then return a video block (sound included) instead of frames.
func WithVideo(ctx context.Context) context.Context {
	return context.WithValue(ctx, videoKey{}, true)
}

// TakesVideo reports whether ctx came from WithVideo.
func TakesVideo(ctx context.Context) bool {
	v, _ := ctx.Value(videoKey{}).(bool)
	return v
}

type wakeKey struct{}

// WithWake returns a context carrying a channel that is closed when the
// user sends a message to the running turn. A tool that only waits (for
// background tasks, for a shell's output) stops waiting then, so the
// message reaches the model without sitting out the whole wait.
func WithWake(ctx context.Context, ch <-chan struct{}) context.Context {
	return context.WithValue(ctx, wakeKey{}, ch)
}

// Wake is ctx's wake channel; nil (never ready) without one.
func Wake(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(wakeKey{}).(<-chan struct{})
	return ch
}

// Woken reports whether ctx's wake channel is closed.
func Woken(ctx context.Context) bool {
	select {
	case <-Wake(ctx):
		return true
	default:
		return false
	}
}

// WokenNote ends the output of a wait the user's message cut short.
const WokenNote = "Stopped waiting: the user sent a message, which follows. Wait again after it if you still need to."
