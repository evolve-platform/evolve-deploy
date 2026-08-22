package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/evolve-platform/evolve-deploy/internal/tmpl"
)

// plugins are the actions a `uses` may name.
//
// A plain map rather than registration from an init: the set is known at
// compile time, and an unknown name has to be able to list the known ones.
// `cmd` is deliberately absent — it is a field of its own, not an action to
// look up, so `uses: cmd` is as wrong as `uses: honycomb`.
var plugins = map[string]func(*yaml.Node) (Action, error){
	"honeycomb": parseHoneycomb,
	"http":      parseHTTP,
	"sentry":    parseSentry,
}

// An Action is one thing a hook does: a shell command, a marker, a check.
type Action interface {
	// Describe is the one line the plan prints. On an action as the file wrote
	// it the variables are still in it — there is no url until something is
	// staged, so a plan has nothing to render with.
	Describe() string

	// Render substitutes the variables and returns what will run. Planning
	// calls it too and throws the result away; see Validate for why.
	Render(data any, funcs template.FuncMap) (Step, error)
}

// A Step is a rendered action, ready to run.
//
// A struct rather than an interface so that Run is only reachable through
// Render. An action whose variables are still in it must not be able to reach
// Honeycomb: a marker reading `purchase {{.version}}` is worse than no marker,
// because it looks like it worked.
type Step struct {
	line string
	run  func(context.Context, *Exec) error
}

// Line is what the step will do, with its variables filled in.
func (s Step) Line() string { return s.line }

// Run does the thing.
func (s Step) Run(ctx context.Context, e *Exec) error { return s.run(ctx, e) }

// noted is a failure that is reported and then forgiven.
//
// The Runner prints it and carries on to the next hook. A deploy marker is what
// needs it: an `after` hook runs on a release that has already succeeded, and
// pulling a working version because a note about it did not arrive costs more
// than the missing note. Every one of these calls was already doing exactly
// this by hand, as `|| echo "marker not recorded"` — which is a policy worth
// having in one place instead of at the end of every command line.
type noted struct{ err error }

// Noted marks err as worth reporting and not worth failing over.
func Noted(err error) error {
	if err == nil {
		return nil
	}
	return noted{err: err}
}

func (n noted) Error() string { return n.err.Error() }
func (n noted) Unwrap() error { return n.err }

// A Checker is an action that can refuse before anything is deployed.
//
// An API key that is not in the environment is not something to find out from
// an `after` hook, which runs on a release that has already succeeded.
type Checker interface {
	Check() error
}

// Exec is where a step runs and where its output goes.
//
// The Runner owns everything around it — holding output on a green run,
// tagging every line with the service it came from, timing each step — so no
// action has to repeat any of it.
type Exec struct {
	// Dir is the working directory, normally the repository root.
	Dir string
	// Out receives whatever the step has to say. Write whole lines: the
	// Runner tags them, and a half line from one service lands in the middle
	// of another's.
	Out io.Writer
}

// Hook is one entry in before, after or smoke.
//
// It is either a command line — a plain string, or `cmd:` — or a named action
// with its options:
//
//	before:
//	  - hive schema:check --commit {{.version}}
//	  - cmd: hive schema:check --commit {{.version}}
//	  - uses: http
//	    with: { url: '{{url "site"}}/health', retry: 5 }
type Hook struct {
	action Action
	err    error
}

// Command builds a hook that runs a shell line. It is what a plain string in
// the file becomes, and how a test writes one without going through YAML.
func Command(line string) *Hook {
	return &Hook{action: cmdAction{line: line}}
}

// UnmarshalYAML reads an entry and keeps whatever went wrong for later.
//
// The error is deliberately not returned. Every mistake an entry can contain —
// both `cmd` and `uses`, an action nobody has, an option it does not take —
// belongs in the config's own "is invalid" list next to the path that names
// it, and a decoder error is neither of those. So the entry parses either way
// and Action reports the failure once validation can say where it was.
func (h *Hook) UnmarshalYAML(n *yaml.Node) error {
	h.action, h.err = parse(n)
	return nil
}

// Action is what this hook does, or why it is not a hook at all.
func (h *Hook) Action() (Action, error) { return h.action, h.err }

// Describe is the one line the plan prints for this hook.
func (h *Hook) Describe() string {
	if h.action == nil {
		// Unreachable through the CLI: validation refuses the file before
		// anything renders it. Better than a nil dereference all the same.
		return "(invalid hook)"
	}
	return h.action.Describe()
}

// Check reports what is wrong with these hooks, one message per entry, each
// prefixed with the path that names it.
//
// It is what puts a hook mistake in the same sorted list as every other config
// mistake, rather than in a decoder error with a line number and no context.
func Check(where string, hs []*Hook) []string {
	var msgs []string
	for i, h := range hs {
		if _, err := h.Action(); err != nil {
			msgs = append(msgs, fmt.Sprintf("%s[%d]: %v", where, i, err))
		}
	}
	return msgs
}

// parse reads one entry, and puts the line it was on in front of whatever went
// wrong with it.
//
// In one place, so that every message about a hook is shaped the same way. The
// path validation prints — services.site.after[1] — already says which entry;
// the line is what an editor can be sent to.
func parse(n *yaml.Node) (Action, error) {
	a, err := parseNode(n)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", n.Line, err)
	}
	return a, nil
}

func parseNode(n *yaml.Node) (Action, error) {
	switch n.Kind {
	case yaml.ScalarNode:
		// The original form, and still the shortest way to write the common
		// case. Every config in existence is made of these.
		return cmdAction{line: n.Value}, nil
	case yaml.MappingNode:
		return parseEntry(n)
	default:
		return nil, fmt.Errorf("a hook is either a command line or "+
			"a `uses` block, not a %s", kindOf(n))
	}
}

func parseEntry(n *yaml.Node) (Action, error) {
	var (
		cmd, uses    string
		hasCmd, seen bool
		with         *yaml.Node
	)
	// Walked by hand rather than decoded into a struct because yaml's
	// KnownFields does not reach inside a custom unmarshaller: decoded, a
	// misspelt `use:` would be silently ignored and the hook would look like
	// it had neither field.
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		switch key.Value {
		case "cmd":
			cmd, hasCmd = val.Value, true
		case "uses":
			uses, seen = val.Value, true
		case "with":
			with = val
		default:
			return nil, fmt.Errorf("%q is not a hook field "+
				"(want cmd, uses or with)", key.Value)
		}
	}

	switch {
	case hasCmd && seen:
		return nil, errors.New("set either `cmd` or `uses`, not both")
	case hasCmd && with != nil:
		return nil, errors.New("`with` names the options of a `uses`, " +
			"and a `cmd` is a whole command line already")
	case hasCmd:
		return cmdAction{line: cmd}, nil
	case !seen:
		return nil, errors.New("needs either `cmd` (a command line) " +
			"or `uses` (a named action)")
	}

	parse, ok := plugins[uses]
	if !ok {
		return nil, fmt.Errorf("uses: %q is not one of %s",
			uses, strings.Join(slices.Sorted(maps.Keys(plugins)), ", "))
	}
	return parse(with)
}

// decode reads an action's options out of its `with` block.
//
// Strictly: an option nobody has is a mistake, and one that is quietly ignored
// is a marker that never appears with nothing to say why. KnownFields does not
// reach in here from the file's own decoder, so the node goes back through a
// decoder of its own.
func decode(n *yaml.Node, out any) error {
	if n == nil {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("`with` is a block of options, not a %s", kindOf(n))
	}

	b, err := yaml.Marshal(n)
	if err != nil {
		return fmt.Errorf("with: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("with: %w", explainDecode(err))
	}
	return nil
}

// notFound matches what yaml.v3 says about an option that does not exist.
var notFound = regexp.MustCompile(`field (\S+) not found in type \S+`)

// explainDecode rewrites yaml's own wording for an unknown option.
//
// "field mesage not found in type hooks.honeycombOptions" names a Go type the
// reader has never heard of and cannot go and look at. The option they wrote is
// the only part of that sentence they can act on.
func explainDecode(err error) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return err
	}
	msgs := make([]string, 0, len(typeErr.Errors))
	for _, msg := range typeErr.Errors {
		if m := notFound.FindStringSubmatch(msg); m != nil {
			msgs = append(msgs, fmt.Sprintf("no such option: %s", m[1]))
			continue
		}
		msgs = append(msgs, msg)
	}
	return errors.New(strings.Join(msgs, "; "))
}

// render substitutes the variables in every field an action was given, so a
// typo in one of them is found in the same pass as a typo in the others.
func render(data any, funcs template.FuncMap, fields ...*string) error {
	for _, f := range fields {
		if *f == "" {
			continue
		}
		out, err := tmpl.RenderWith(*f, data, funcs)
		if err != nil {
			return err
		}
		*f = out
	}
	return nil
}

func kindOf(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return "value"
	case yaml.MappingNode:
		return "block"
	case yaml.SequenceNode:
		return "list"
	default:
		return "value"
	}
}
