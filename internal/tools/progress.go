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
