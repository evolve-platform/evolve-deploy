package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// riderMove is one service whose traffic moved, and the version that is now
// serving it. Its riders follow that version.
type riderMove struct {
	service string
	// version is empty when the moved targets could not say which version they
	// serve, or did not agree on one; problem says which.
	version string
	problem string
}

// ridersOf is every target of a service that carries no traffic: the jobs and
// workers a release writes at the switch.
func ridersOf(f *config.File, r target.Rollout, service string) []*config.Target {
	c := f.Services[service]
	if c == nil {
		return nil
	}
	var out []*config.Target
	for _, t := range c.Targets {
		if !r.Routable(t) {
			out = append(out, t)
		}
	}
	return out
}

// servedVersion is the version a label holds across a service's targets.
//
// Read from the traffic block after the move, which is the same record the
// rollback plan was made from: there is nothing else to read, and nothing
// remembered to read it from instead.
func servedVersion(
	ctx context.Context,
	r target.Rollout,
	service string,
	targets []*config.Target,
	label string,
) riderMove {
	versions := map[string]bool{}
	for _, t := range targets {
		entries, err := r.Traffic(ctx, t)
		if err != nil {
			return riderMove{service: service, problem: fmt.Sprintf(
				"the version %s serves could not be read: %v", label, err)}
		}
		for _, e := range entries {
			if e.Label == label {
				versions[e.Version] = true
			}
		}
	}

	if len(versions) != 1 || versions[""] {
		return riderMove{service: service, problem: fmt.Sprintf(
			"%s does not hold one known version on every target of %s", label, service)}
	}
	for v := range versions {
		return riderMove{service: service, version: v}
	}
	return riderMove{service: service}
}

// restoreRiders brings each moved service's riders to the version it now serves.
//
// After the traffic, never before it: the traffic is the half that decides
// whether users see the broken version, and it is one write, so it goes first and
// nothing here can hold it up. A rider that then fails does not move the traffic
// back. It fails the command, naming the rider and the deploy that finishes the
// job, because the environment is now on the old version everywhere except there.
//
// A rider this driver cannot restore is not a failure of the command: it is a
// known limit, so it is listed with the same deploy, and the traffic having
// moved is still reported as the success it is.
func restoreRiders(
	ctx context.Context,
	out io.Writer,
	driver target.Driver,
	r target.Rollout,
	f *config.File,
	moves []riderMove,
) error {
	type pending struct {
		t    *config.Target
		move riderMove
	}
	var todo []pending
	for _, m := range moves {
		for _, t := range ridersOf(f, r, m.service) {
			todo = append(todo, pending{t, m})
		}
	}
	if len(todo) == 0 {
		return nil
	}

	fmt.Fprintf(out, "\nBringing %d rider(s) to the version their service now serves:\n", len(todo))

	restorer, _ := driver.(target.Restorer)
	var problems, notMoved []string
	for _, p := range todo {
		t, m := p.t, p.move
		redeploy := fmt.Sprintf("evolve-deploy apply %s --only %s --set %s=%s",
			f.Path, m.service, m.service, versionOrPlaceholder(m.version))

		if m.problem != "" {
			fmt.Fprintf(out, "  %-40s not moved: %s\n", t.Label(), m.problem)
			notMoved = append(notMoved, redeploy)
			continue
		}
		if restorer == nil {
			fmt.Fprintf(out, "  %-40s not moved: %s cannot put a rider back on a version\n",
				t.Label(), driver.Name())
			notMoved = append(notMoved, redeploy)
			continue
		}

		ch, err := restorer.PlanRestore(ctx, t, m.version)
		var unsupported *target.ErrNotImplemented
		switch {
		case errors.As(err, &unsupported):
			fmt.Fprintf(out, "  %-40s not moved: %s has no way back to a version\n", t.Label(), t.Type)
			notMoved = append(notMoved, redeploy)
			continue
		case err != nil:
			fmt.Fprintf(out, "  %-40s failed: %v\n", t.Label(), err)
			problems = append(problems, fmt.Sprintf("%s: %v\n      instead: %s", t.Label(), err, redeploy))
			continue
		case ch == nil:
			fmt.Fprintf(out, "  %-40s already on %s\n", t.Label(), m.version)
			continue
		}

		// Before rather than after, as everywhere else here: a worker waits for
		// its revision to be ready, and that wait is the silence the line explains.
		fmt.Fprintf(out, "  %-40s %s\n", t.Label(), ch.Reason)
		if err := driver.Apply(ctx, ch); err != nil {
			fmt.Fprintf(out, "  %-40s failed: %v\n", t.Label(), err)
			problems = append(problems, fmt.Sprintf("%s: %v\n      instead: %s", t.Label(), err, redeploy))
			continue
		}
		fmt.Fprintf(out, "  %-40s now on %s\n", t.Label(), m.version)
	}

	if len(notMoved) > 0 {
		fmt.Fprintf(out, "\nThe riders above that did not move are still on the version they were last\n"+
			"released with. A deploy puts them on the one serving:\n  %s\n",
			strings.Join(dedupe(notMoved), "\n  "))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("the traffic moved, but %d rider(s) did not follow it:\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// showRiders prints a service's riders under its traffic, with the version
// each one runs, so a rider left behind by a rollback is visible next to the
// version the traffic is on.
func showRiders(ctx context.Context, out io.Writer, driver target.Driver, r target.Rollout, f *config.File, service string) {
	riders := ridersOf(f, r, service)
	if len(riders) == 0 {
		return
	}
	restorer, _ := driver.(target.Restorer)
	for _, t := range riders {
		version := "(not read)"
		if restorer != nil {
			v, err := restorer.RiderVersion(ctx, t)
			switch {
			case err != nil:
				version = "(could not be read)"
			default:
				version = target.VersionOrUnknown(v)
			}
		}
		fmt.Fprintf(out, "  %-8s %-40s %s\n", "rider", t.Label(), version)
	}
}

// notMovedAfterUndo lists the riders of services whose release the platform
// reversed. Undo says nothing about which version it went back to, so there is
// no version to restore them to here; the deploy is the way.
func notMovedAfterUndo(out io.Writer, r target.Rollout, f *config.File, services []string) {
	var lines, redeploys []string
	for _, s := range services {
		for _, t := range ridersOf(f, r, s) {
			lines = append(lines, t.Label())
			redeploys = append(redeploys, fmt.Sprintf(
				"evolve-deploy apply %s --only %s --set %s=<previous version>", f.Path, s, s))
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(out, "\nNot moved, still on the version they were last released with:\n  %s\n"+
		"A deploy puts them back:\n  %s\n",
		strings.Join(lines, "\n  "), strings.Join(dedupe(redeploys), "\n  "))
}

func versionOrPlaceholder(v string) string {
	if v == "" {
		return "<version>"
	}
	return v
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
