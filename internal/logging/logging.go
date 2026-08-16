// Package logging sets up debug output.
//
// The default is silence: a deploy log should show what is being deployed, not
// how the tool talks to an API. With --verbose it shows every call it makes and
// every poll while waiting, which is what you want the first time it runs
// against a real environment and the only thing you have is "it is hanging".
//
// Values are never logged — only names, references and states. A debug flag
// must not be a way to print a secret into a pipeline log.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// Setup installs the default logger. Debug output goes to stderr so it stays
// out of anything that pipes the tool's normal output.
func Setup(w io.Writer, verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(&handler{w: w, level: level}))
}

// handler prints one line per record: a wall-clock time, the message, and the
// attributes. slog's own TextHandler is machine-readable at the cost of being
// hard to scan, and this output is meant for someone watching a deploy.
type handler struct {
	w     io.Writer
	level slog.Level
	attrs []slog.Attr
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s", r.Time.Format("15:04:05"), r.Message)

	write := func(a slog.Attr) bool {
		fmt.Fprintf(&b, "  %s=%v", a.Key, a.Value)
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)

	b.WriteByte('\n')
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}

func (h *handler) WithGroup(string) slog.Handler { return h }

// Timer reports how long something took, which is the number you want when a
// deploy feels slow: the rollout itself, or everything around it.
type Timer struct {
	start time.Time
	msg   string
	attrs []any
}

// Start begins timing an operation.
func Start(msg string, attrs ...any) *Timer {
	slog.Debug(msg, attrs...)
	return &Timer{start: time.Now(), msg: msg, attrs: attrs}
}

// Done logs the elapsed time.
func (t *Timer) Done(attrs ...any) {
	all := append(append([]any{}, t.attrs...), attrs...)
	all = append(all, "took", time.Since(t.start).Round(time.Millisecond))
	slog.Debug(t.msg+" done", all...)
}
