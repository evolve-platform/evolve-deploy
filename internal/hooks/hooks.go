// Package hooks runs the before, after and smoke steps of a release.
//
// A hook is either a command line or one of a small set of named actions. The
// command line came first and is still the whole of the contract: deploy-time
// gates belong next to the deploy, and the tool has no business knowing what
// `hive schema:publish` is.
//
// The named actions exist for the ones that were never really commands. A
// Honeycomb marker written as curl is six lines of flags, a hand-built JSON
// body, a header out of an environment variable and a `|| echo` on the end so
// that a failed annotation does not fail a release — and every value in it is
// something the tool already knows: the version, the service, the environment,
// the side. An action that is given those and can refuse at plan time, when the
// key is missing rather than after the deploy, is worth the tool knowing about.
//
// So the set stays small and stays about what a deploy is — say a version went
// out, ask whether something answers — and `cmd` covers everything else.
package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"text/template"

	"github.com/evolve-platform/evolve-deploy/internal/logging"
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
	// Verbose streams what a hook prints, and times each one.
	//
	// Off, a hook that succeeds says nothing. A release runs a schema check, a
	// publish and a smoke test per service, each of which is a CLI that prints a
	// screenful, and none of it is the answer to what was deployed — which is six
	// lines it would otherwise be buried under. A hook that *fails* still prints
	// everything it printed, because that output is the diagnosis.
	Verbose bool

	// mu serialises writes to Out. Every service's hooks write whole lines,
	// but whole lines from two processes still land on top of each other
	// without one lock between them.
	mu sync.Mutex
}

// Vars are the substitutions available in a hook: {{.version}}, {{.name}},
// {{.env}}.
type Vars map[string]string

// Run executes the hooks of one service in order and stops at the first
// failure. Everything they print is tagged with the service name.
func (r *Runner) Run(ctx context.Context, service, phase string, hooks []*Hook, vars Vars) error {
	return r.RunWith(ctx, service, phase, hooks, vars.Data(), nil)
}

// Data is Vars as RunWith and ValidateWith take it, for a hook that is given
// functions as well as variables.
func (v Vars) Data() map[string]any {
	d := make(map[string]any, len(v))
	for k, val := range v {
		d[k] = val
	}
	return d
}

// RunWith is Run for hooks whose variables are not flat strings.
//
// The release-wide smoke test needs it: there is no single URL to give a test
// that covers a whole deploy, so it names services instead — {{.site.url}}, or
// the url function where the name has a hyphen in it.
func (r *Runner) RunWith(
	ctx context.Context,
	service, phase string,
	hooks []*Hook,
	data any,
	funcs template.FuncMap,
) error {
	if len(hooks) == 0 {
		return nil
	}

	// A dry run has no concurrency and prints under the service's own heading
	// in the plan, so it keeps the plan's indentation and needs no tag.
	var out *prefixWriter
	if !r.DryRun {
		out = r.writer(service)
		defer out.flush()
	}

	for _, hook := range hooks {
		action, err := hook.Action()
		if err != nil {
			// Refused while the config was read, so this is unreachable from
			// the CLI. Reported rather than skipped all the same: a hook that
			// silently does not run is the failure that is hardest to see.
			return fmt.Errorf("%s hook: %w", phase, err)
		}

		step, err := action.Render(data, funcs)
		if err != nil {
			return fmt.Errorf("%s hook: %w", phase, err)
		}

		if r.DryRun {
			fmt.Fprintf(r.Out, "    %s: %s\n", phase, step.Line())
			continue
		}

		timer := logging.Start("run hook", "service", service, "phase", phase,
			"step", step.Line())

		// Held rather than dropped: output nobody wants on a green run is the
		// only account of a red one.
		var held bytes.Buffer
		sink := io.Writer(out)
		if !r.Verbose {
			sink = &held
		}

		var note noted
		switch err := step.Run(ctx, &Exec{Dir: r.Dir, Out: sink}); {
		case err == nil:
		case errors.As(err, &note):
			// Straight to Out rather than through the sink, because this is the
			// one thing a quiet run still has to say: an annotation that went
			// missing leaves nothing else behind to notice it by.
			fmt.Fprintf(out, "%s\n", note)
		default:
			if held.Len() > 0 {
				// Through the prefix writer, so a failure in one service's hook
				// is still attributed when three ran at once.
				_, _ = out.Write(held.Bytes())
			}
			return fmt.Errorf("%s hook failed: %s: %w", phase, step.Line(), err)
		}
		timer.Done()
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

// Validate renders every hook without running any, and asks the ones that can
// refuse whether they would.
//
// It exists because of `after`. A hook naming a variable that does not exist is
// an error either way — tmpl.Render runs with missingkey=error, so a hook can
// never quietly become `--target tst-` — but that error would otherwise surface
// while the hook runs, and an `after` hook runs on a release that has already
// succeeded. A typo in a variable name must not turn a working deploy into a
// red pipeline, so the rendering is checked during planning, where every other
// findable failure is found.
func Validate(hooks []*Hook, vars Vars) error {
	return ValidateWith(hooks, vars.Data(), nil)
}

// ValidateWith is Validate for the nested data a release-wide smoke test uses.
func ValidateWith(hooks []*Hook, data any, funcs template.FuncMap) error {
	for _, hook := range hooks {
		action, err := hook.Action()
		if err != nil {
			return err
		}
		if _, err := action.Render(data, funcs); err != nil {
			return err
		}
		// The same argument one step further out. A marker with no API key
		// anywhere is a hook that cannot work, and finding that out from an
		// `after` hook means finding it out from a release that succeeded.
		if c, ok := action.(Checker); ok {
			if err := c.Check(); err != nil {
				return err
			}
		}
	}
	return nil
}
