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

// renderGate says what stands between staging and the traffic moving.
//
// Once, at the end, because that is what it is. Printed against each target it
// read as though every app ran the whole suite — four targets and three commands
// looking like twelve runs — which is exactly the misreading that moving the
// gate up a level was meant to end.
func renderGate(w io.Writer, p *plan.Plan) {
	if !p.Staging() {
		return
	}

	side, previous := p.Side()
	smoke := p.SmokeHooks()
	if len(smoke) == 0 {
		fmt.Fprintf(w, "The whole of %s is staged, then switched. No smoke steps, "+
			"so nothing is checked in between.\n", side)
		return
	}

	fmt.Fprintf(w, "Once all of %s is staged, %s before any traffic moves:\n",
		side, pluralise(len(smoke), "smoke step"))
	for _, hook := range smoke {
		fmt.Fprintf(w, "  %s\n", hook.Describe())
	}
	fmt.Fprintf(w, "A failure there switches nothing: %s keeps serving.\n", previous)
}

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
			// A carried target is staged at the version it already runs, so
			// "a8cb13c -> a8cb13c" would read as a mistake. What is worth saying
			// is why it is in the release at all.
			if ch.Carry {
				fmt.Fprintf(w, "  %-*s %s, staged to complete the side\n", width,
					ch.Target.Label(), ch.ToVersion)
			} else {
				fmt.Fprintf(w, "  %-*s %s -> %s\n", width,
					ch.Target.Label(),
					orNone(ch.FromVersion), ch.ToVersion)
			}

			// The reason is only worth a line when it says something the
			// versions above do not. "version a -> b" next to "a -> b" is half
			// the screen spent on nothing.
			if r := ch.Reason; r != "" && r != versionReason(ch) {
				fmt.Fprintf(w, "  %-*s %s\n", width, "", r)
			}
			renderRollout(w, width, cp, ch)
			renderEnv(w, ch)
		}

		for _, t := range cp.Unchanged {
			fmt.Fprintf(w, "  %-*s up to date\n", width, t.Label())
		}

		// Ordering is invisible in the plan otherwise, and a service that sits
		// there doing nothing for two minutes looks stuck rather than queued.
		if deps := cp.Service.DependsOn; len(deps) > 0 {
			fmt.Fprintf(w, "  %-*s %s\n", width, "waits for", strings.Join(deps, ", "))
		}

		if len(cp.Service.Before) > 0 || len(cp.Service.After) > 0 {
			fmt.Fprintf(w, "  %-*s %d before, %d after\n", width, "hooks",
				len(cp.Service.Before), len(cp.Service.After))
		}
	}

	fmt.Fprintln(w)
	renderGate(w, p)
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

// renderRollout says how this target is going out, and what it falls back to.
//
// Worth two lines because with a strategy in the config the shape of a release
// is a choice rather than a given, and because "what do I roll back to" is the
// question a blue-green deploy raises and nothing else answers.
func renderRollout(w io.Writer, width int, cp *plan.ServicePlan, ch *target.Change) {
	if ch.Sides == nil {
		return
	}

	fmt.Fprintf(w, "  %-*s blue-green: stage %s, then switch\n", width, "",
		ch.Sides.Idle.Label)

	// The environment diff above cannot show these — they differ by side by
	// definition and are excluded from it — so without a line here the deploy
	// would rewrite a downstream URL with nothing on screen saying so.
	if names := cp.Service.Strategy.SideEnvNames(); len(names) > 0 {
		fmt.Fprintf(w, "  %-*s %s environment: %s\n", width, "",
			ch.Sides.Idle.Label, strings.Join(names, ", "))
	}

	// What this falls back to, and whether that fallback is still there, is the
	// thing worth knowing before pressing go. Naming it is the difference
	// between trusting it and hoping so — and the three answers are genuinely
	// different, so there are three lines rather than one hedged one.
	fmt.Fprintf(w, "  %-*s rollback is %s %s (%s)\n", width, "",
		ch.Sides.Active.Label, orNone(ch.FromVersion), ch.Fallback)
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
