package hooks

import (
	"context"
	"os/exec"
	"strings"
	"text/template"
)

// cmdAction is a shell command line, and the reason the rest of the actions can
// stay a small set: anything the tool has no opinion about is still one of
// these.
type cmdAction struct {
	line string
}

// Describe unfolds a YAML block scalar so a command written over three lines of
// curl flags prints as the one thing it is.
//
// Not truncated: the interesting part of a command — the path it asks for, the
// commit it publishes — is at the end, which is exactly what a width limit
// would cut off.
func (a cmdAction) Describe() string {
	return strings.Join(strings.Fields(a.line), " ")
}

func (a cmdAction) Render(data any, funcs template.FuncMap) (Step, error) {
	b := a
	if err := render(data, funcs, &b.line); err != nil {
		return Step{}, err
	}
	return Step{line: b.Describe(), run: b.run}, nil
}

func (a cmdAction) run(ctx context.Context, e *Exec) error {
	// Through a shell, because a hook is written the way it would be typed:
	// pipes, quoting and $VARS all work as expected.
	cmd := exec.CommandContext(ctx, "sh", "-c", a.line)
	cmd.Dir = e.Dir
	cmd.Stdout = e.Out
	cmd.Stderr = e.Out
	return cmd.Run()
}
