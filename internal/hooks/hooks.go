// Package hooks runs the before and after commands of a service.
//
// The tool knows nothing about Hive, Sentry or anything else: a hook is a shell
// command. That keeps deploy-time gates — check a schema before rolling out,
// publish it after — where they belong, without teaching the deploy tool about
// every integration a project happens to use.
package hooks

import (
	"context"
	"fmt"
	"io"
	"os/exec"

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
}

// Vars are the substitutions available in a hook: {{.version}}, {{.name}},
// {{.env}}.
type Vars map[string]string

// Run executes the commands in order and stops at the first failure.
func (r *Runner) Run(ctx context.Context, phase string, commands []string, vars Vars) error {
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
		cmd.Stdout = r.Out
		cmd.Stderr = r.Out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s hook failed: %s: %w", phase, line, err)
		}
	}
	return nil
}
