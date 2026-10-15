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

type progressWriter func([]byte)

func (f progressWriter) Write(p []byte) (int, error) {
	f(p)
	return len(p), nil
}
