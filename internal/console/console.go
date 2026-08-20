// Package console writes the tool's human-facing output.
//
// Everything a deploy prints goes through one Log, for two reasons. The first is
// attribution: services roll out at the same time, so a line that does not name
// the service it came from is worth very little in a pipeline log where five of
// them are interleaved. Every line carries a [service] tag.
//
// The second is that a column only lines up if one place decides how wide it
// is. The tag and the label are padded to widths measured from the plan before
// anything is printed, so the plan, the hook output, the progress lines and the
// failures all form the same two columns.
package console

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// Log is a writer that tags and aligns.
//
// Every method takes the lock, so lines from concurrent services never land on
// top of each other.
type Log struct {
	out io.Writer
	mu  sync.Mutex

	// tag is the width of the [service] column, brackets included; label the
	// width of the column after it. Both are zero when there is nothing to line
	// up — a single service, or output that has no plan behind it.
	tag   int
	label int
}

// New returns a Log writing to w. The widths are the two column sizes, as
// measured from the plan; see ui.NewLog, which is what callers use.
func New(w io.Writer, tag, label int) *Log {
	return &Log{out: w, tag: tag, label: label}
}

// Line writes a tagged line with a label in its own column: a target and what
// happened to it.
func (l *Log) Line(service, label, format string, args ...any) {
	l.write(l.prefix(service) + fmt.Sprintf("%-*s ", l.label, label) +
		fmt.Sprintf(format, args...))
}

// Note writes a tagged line that is about the service rather than one of its
// targets, so it starts where the labels do and runs on from there.
func (l *Log) Note(service, format string, args ...any) {
	l.write(l.prefix(service) + fmt.Sprintf(format, args...))
}

// Plain writes an untagged line, for the few things that belong to the run as a
// whole rather than to a service.
func (l *Log) Plain(format string, args ...any) {
	l.write(fmt.Sprintf(format, args...))
}

// Blank separates one block of output from the next.
func (l *Log) Blank() { l.write("") }

// prefix renders the [service] column. An empty service still takes the space,
// so a continuation line stays under the ones it belongs to.
func (l *Log) prefix(service string) string {
	if service == "" {
		return fmt.Sprintf("%-*s ", l.tag, "")
	}
	tag := "[" + service + "]"
	return fmt.Sprintf("%-*s ", max(l.tag, len(tag)), tag)
}

func (l *Log) write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintln(l.out, line)
}

// Writer returns an io.Writer that tags every line a subprocess prints with the
// service it came from.
//
// Hooks for several services run at the same time, and a hook is usually a
// package manager that prints a great deal. Untagged, the log is three installs
// shredded into each other with no way to tell whose "Detected 4 errors" that
// was — and because a process writes when it feels like it, not in whole lines,
// one line can be cut in half by another service mid-word.
//
// So output is buffered until a line is complete and then written through the
// Log's lock. A line is always whole, and always attributed. Close writes
// whatever was left behind without a trailing newline, which would otherwise be
// swallowed — and that is exactly where a prompt or a last error tends to sit.
func (l *Log) Writer(service string) io.WriteCloser {
	return &writer{log: l, service: service}
}

type writer struct {
	log     *Log
	service string
	mu      sync.Mutex
	buf     []byte
}

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.log.Note(w.service, "%s", w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.buf) > 0 {
		w.log.Note(w.service, "%s", w.buf)
		w.buf = nil
	}
	return nil
}
