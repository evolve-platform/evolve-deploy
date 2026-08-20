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

	"github.com/evolve-platform/evolve-deploy/internal/console"
	"github.com/evolve-platform/evolve-deploy/internal/plan"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// NewLog returns the log every line of a run is written through, with its
// columns sized to the plan it is about to print.
//
// Sizing happens once, here, because the plan and everything that follows it —
// hook output, progress lines, failures — only read as columns if they agree on
// how wide those columns are.
func NewLog(w io.Writer, p *plan.Plan) *console.Log {
	return console.New(w, tagWidth(p), labelWidth(p))
}

// RenderPlan writes a human-readable plan.
//
// There is no per-service heading. Every line names its service in the tag
// already, and the version it is going to is on each target's line, so a
// heading would only repeat both.
func RenderPlan(l *console.Log, p *plan.Plan) {
	if p.Empty() {
		l.Plain("Nothing to do — everything in %s already runs the version it names.", p.File.Path)
		return
	}

	l.Blank()

	for _, cp := range p.Services {
		if !cp.HasWork() {
			continue
		}
		name := cp.Service.Name

		for _, ch := range cp.Changes {
			l.Line(name, ch.Target.Label(), "%s -> %s", orNone(ch.FromVersion), ch.ToVersion)

			// The reason is only worth a line when it says something the
			// versions above do not. "version a -> b" next to "a -> b" is half
			// the screen spent on nothing.
			if r := ch.Reason; r != "" && r != versionReason(ch) {
				l.Line(name, "", "%s", r)
			}
			renderEnv(l, name, ch)
		}

		for _, t := range cp.Unchanged {
			l.Line(name, t.Label(), "up to date")
		}

		// Ordering is invisible in the plan otherwise, and a service that sits
		// there doing nothing for two minutes looks stuck rather than queued.
		if deps := cp.Service.DependsOn; len(deps) > 0 {
			l.Line(name, "waits for", "%s", strings.Join(deps, ", "))
		}

		if len(cp.Service.Before) > 0 || len(cp.Service.After) > 0 {
			l.Line(name, "hooks", "%d before, %d after",
				len(cp.Service.Before), len(cp.Service.After))
		}
	}
	l.Blank()
}

// labelWidth sizes the target column to the longest name actually present. A
// fixed width looked fine with two targets and falls apart at thirteen.
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

// tagWidth sizes the [service] column. Only services with work count: a name
// that is never printed must not indent the ones that are.
func tagWidth(p *plan.Plan) int {
	var width int
	for _, cp := range p.Services {
		if cp.HasWork() {
			width = max(width, len(cp.Service.Name)+2)
		}
	}
	return width
}

func versionReason(ch *target.Change) string {
	return "version " + orNone(ch.FromVersion) + " -> " + ch.ToVersion
}

func renderEnv(l *console.Log, service string, ch *target.Change) {
	for _, name := range ch.EnvAdded {
		l.Line(service, "", "+ %s", name)
	}
	for _, name := range ch.EnvChanged {
		l.Line(service, "", "~ %s", name)
	}
	for _, name := range ch.EnvRemoved {
		l.Line(service, "", "- %s", name)
	}
}

// Summary is the one-line result, for the end of a pipeline log.
func Summary(l *console.Log, p *plan.Plan) {
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
	l.Plain("%s in %s", pluralise(targets, "target"), pluralise(services, "service"))
}

func pluralise(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}
