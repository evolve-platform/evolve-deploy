// Package ui renders a plan.
//
// The output has one job beyond looking tidy: say why. A rollout can be caused
// by something that is not visible in the deploy config — Terraform registering
// a new base revision with more memory — and without a reason on the line, that
// looks like the tool acting on its own.
package ui

import (
	"fmt"
	"io"
	"strings"

	"github.com/evolve-platform/evolve-deploy/internal/plan"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// RenderPlan writes a human-readable plan.
func RenderPlan(w io.Writer, p *plan.Plan) {
	if p.Empty() {
		fmt.Fprintf(w, "Nothing to do — everything in %s already runs the version it names.\n", p.File.Path)
		return
	}

	width := labelWidth(p)

	for _, cp := range p.Services {
		if !cp.HasWork() {
			continue
		}
		fmt.Fprintf(w, "\n%s  %s\n", cp.Service.Name, cp.Service.Version)

		for _, ch := range cp.Changes {
			fmt.Fprintf(w, "  %-*s %s -> %s\n", width,
				ch.Target.Label(),
				orNone(ch.FromVersion), ch.ToVersion)

			// The reason is only worth a line when it says something the
			// versions above do not. "version a -> b" next to "a -> b" is half
			// the screen spent on nothing.
			if r := ch.Reason; r != "" && r != versionReason(ch) {
				fmt.Fprintf(w, "  %-*s %s\n", width, "", r)
			}
			renderEnv(w, ch)
		}

		for _, t := range cp.Unchanged {
			fmt.Fprintf(w, "  %-*s up to date\n", width, t.Label())
		}

		if len(cp.Service.Before) > 0 || len(cp.Service.After) > 0 {
			fmt.Fprintf(w, "  %-*s %d before, %d after\n", width, "hooks",
				len(cp.Service.Before), len(cp.Service.After))
		}
	}
	fmt.Fprintln(w)
}

// labelWidth sizes the column to the longest name actually present. A fixed
// width looked fine with two targets and falls apart at thirteen.
func labelWidth(p *plan.Plan) int {
	width := 20
	for _, cp := range p.Services {
		for _, ch := range cp.Changes {
			width = max(width, len(ch.Target.Label()))
		}
		for _, t := range cp.Unchanged {
			width = max(width, len(t.Label()))
		}
	}
	return width
}

// Width exposes the plan's column width so progress lines match it.
func Width(p *plan.Plan) int { return labelWidth(p) }

func versionReason(ch *target.Change) string {
	return fmt.Sprintf("version %s -> %s", orNone(ch.FromVersion), ch.ToVersion)
}

func renderEnv(w io.Writer, ch *target.Change) {
	for _, name := range ch.EnvAdded {
		fmt.Fprintf(w, "      + %s\n", name)
	}
	for _, name := range ch.EnvChanged {
		fmt.Fprintf(w, "      ~ %s\n", name)
	}
	for _, name := range ch.EnvRemoved {
		fmt.Fprintf(w, "      - %s\n", name)
	}
}

// Summary is the one-line result, for the end of a pipeline log.
func Summary(w io.Writer, p *plan.Plan) {
	var services, targets int
	for _, cp := range p.Services {
		if !cp.HasWork() {
			continue
		}
		services++
		targets += len(cp.Changes)
	}
	if services == 0 {
		return
	}
	fmt.Fprintf(w, "%s in %s\n", pluralise(targets, "target"), pluralise(services, "service"))
}

func pluralise(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}
