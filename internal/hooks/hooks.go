// Package hooks runs the before and after commands of a service.
//
// The tool knows nothing about Hive, Sentry or anything else: a hook is a shell
// command. That keeps deploy-time gates — check a schema before rolling out,
// publish it after — where they belong, without teaching the deploy tool about
// every integration a project happens to use.
package hooks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/evolve-platform/evolve-deploy/internal/tmpl"
)

// Runner executes hook command lines.
type Runner struct {
	// Dir is the working directory, normally the repository root.
	Dir string
	// Out receives the hook's stdout and stderr, so a failing schema check
	// explains itself in the pipeline log.
	Out io.Writer
	// DryRun prints what would run without running it.
	DryRun bool
	// Width pads the [service] tag so hook output lines up in a column. Zero
	// means pad to nothing, which is what a single service wants.
	Width int

	// mu serialises writes to Out. Every service's hooks write whole lines,
	// but whole lines from two processes still land on top of each other
	// without one lock between them.
	mu sync.Mutex
}

// Vars are the substitutions available in a hook: {{.version}}, {{.name}},
// {{.env}}.
type Vars map[string]string

// Run executes the commands of one service in order and stops at the first
// failure. Everything they print is tagged with the service name.
func (r *Runner) Run(ctx context.Context, service, phase string, commands []string, vars Vars) error {
	if len(commands) == 0 {
		return nil
	}

	// A dry run has no concurrency and prints under the service's own heading
	// in the plan, so it keeps the plan's indentation and needs no tag.
	var out *prefixWriter
	if !r.DryRun {
		out = r.writer(service)
		defer out.flush()
	}

	for _, raw := range commands {
		line, err := tmpl.Render(raw, vars)
		if err != nil {
			return fmt.Errorf("%s hook: %w", phase, err)
		}

		if r.DryRun {
			fmt.Fprintf(r.Out, "    %s: %s\n", phase, line)
			continue
		}

		// Through a shell, because a hook is written the way it would be typed:
		// pipes, quoting and $VARS all work as expected.
		cmd := exec.CommandContext(ctx, "sh", "-c", line)
		cmd.Dir = r.Dir
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s hook failed: %s: %w", phase, line, err)
		}
	}
	return nil
}

func (r *Runner) writer(service string) *prefixWriter {
	w := &prefixWriter{mu: &r.mu, out: r.Out}
	if service != "" {
		tag := "[" + service + "]"
		w.prefix = fmt.Sprintf("%-*s ", max(r.Width, len(tag)), tag)
	}
	return w
}

// prefixWriter tags every line a hook prints with the service it came from.
//
// Hooks for several services run at the same time, and a hook is usually a
// package manager that prints a great deal. Untagged, the log is three installs
// shredded into each other with no way to tell whose "Detected 4 errors" that
// was — and because a process writes when it feels like it, not in whole lines,
// one line can be cut in half by another service mid-word.
//
// So output is buffered until a line is complete and then written under a lock
// shared by every writer on the Runner. A line is always whole, and always
// attributed.
type prefixWriter struct {
	mu     *sync.Mutex
	out    io.Writer
	prefix string
	buf    []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		if _, err := fmt.Fprintf(w.out, "%s%s\n", w.prefix, w.buf[:i]); err != nil {
			return 0, err
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// flush writes whatever the hook left behind without a trailing newline, which
// would otherwise be swallowed — and that is exactly where a prompt or a last
// error tends to sit.
func (w *prefixWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.buf) > 0 {
		fmt.Fprintf(w.out, "%s%s\n", w.prefix, w.buf)
		w.buf = nil
	}
}

// Validate renders every command without running any.
//
// It exists because of `after`. A hook naming a variable that does not exist is
// an error either way — tmpl.Render runs with missingkey=error, so a hook can
// never quietly become `--target tst-` — but that error would otherwise surface
// while the hook runs, and an `after` hook runs on a release that has already
// succeeded. A typo in a variable name must not turn a working deploy into a
// red pipeline, so the rendering is checked during planning, where every other
// findable failure is found.
func Validate(commands []string, vars Vars) error {
	for _, raw := range commands {
		if _, err := tmpl.Render(raw, vars); err != nil {
			return err
		}
	}
	return nil
}
