package tools

import "context"

// Jobs runs a tool's long work in the background of the session, the way a
// background task subagent runs: the call returns at once, and the result
// comes to the model in a message when the job ends (an MCP server's task).
type Jobs interface {
	// Start runs run and returns the id the model knows the job by. run
	// gets a context that ends when the job is stopped, and a func to say
	// what it is doing; its text (or error) is the job's report.
	Start(name, desc string, run func(ctx context.Context, progress func(string)) (string, error)) string
}

type jobsKey struct{}

// WithJobs returns a context whose tools may run work in the background
// through j; nil j takes that away (a subagent waits for its work).
func WithJobs(ctx context.Context, j Jobs) context.Context {
	return context.WithValue(ctx, jobsKey{}, j)
}

// JobsFrom is ctx's Jobs, nil without.
func JobsFrom(ctx context.Context) Jobs {
	j, _ := ctx.Value(jobsKey{}).(Jobs)
	return j
}

// Progress is ctx's progress func (WithProgress), nil without.
func Progress(ctx context.Context) func(chunk []byte) { return progressFrom(ctx) }
