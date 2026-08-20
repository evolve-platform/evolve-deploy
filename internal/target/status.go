package target

import (
	"context"
	"fmt"
)

// A deploy is mostly waiting, and the waiting is silent: one write, then a poll
// every few seconds until a container passes its probes. Two minutes of that
// prints nothing, which in a pipeline log is indistinguishable from a hang.
//
// So a target that has been quiet for a while gets a progress line, and a driver
// says what it is waiting for through the context it was handed. The note is
// what makes that line worth reading: "still waiting" only says the process is
// alive, "still waiting on health" says which part of the deploy is slow.
type statusKey struct{}

// WithStatus returns a context that carries note, called by a driver whenever
// what it is waiting for changes.
func WithStatus(ctx context.Context, note func(string)) context.Context {
	return context.WithValue(ctx, statusKey{}, note)
}

// Status records what the driver is now waiting for. It is a no-op when nobody
// is listening, so a driver may call it freely — including from a test, or from
// Plan, where there is no progress line to feed.
func Status(ctx context.Context, format string, args ...any) {
	if note, ok := ctx.Value(statusKey{}).(func(string)); ok {
		note(fmt.Sprintf(format, args...))
	}
}
