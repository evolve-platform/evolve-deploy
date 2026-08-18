package plan

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// defaultConcurrency is set above the service count of any repository this is
// aimed at, so the limit does not quietly serialise a deploy. Nine services
// against a limit of eight cost a full extra wave before anyone noticed.
const defaultConcurrency = 16

// Options configure an apply.
type Options struct {
	Driver target.Driver
	Hooks  *hooks.Runner
	Out    io.Writer
	// Concurrency bounds how many services roll out at once.
	//
	// A worker spends almost all its time waiting: one write, then a poll every
	// few seconds for a minute or two. That is a fraction of an API call per
	// second, and the goroutine costs nothing while it blocks — so this is not
	// about local resources, and only marginally about throttling. It is a
	// backstop for a repository with far more services than anyone has today.
	//
	// Raising it past the number of services does nothing at all.
	Concurrency int
	// Width aligns progress lines with the plan printed above them.
	Width int
}

// Apply executes a plan.
//
// It runs in two phases. Every before hook first, as one gate for the whole
// release; only if all of them pass does anything get written. After that,
// services roll out concurrently and independently: one failing service does
// not cancel another that is already halfway through, because aborting a
// healthy deploy is worse than letting it finish. Within a service the targets
// move together — if one fails, the ones that succeeded are put back.
func Apply(ctx context.Context, p *Plan, o Options) error {
	if o.Concurrency <= 0 {
		o.Concurrency = defaultConcurrency
	}
	if o.Width <= 0 {
		o.Width = 34
	}

	// Hook output is tagged with the service that produced it, in a column
	// sized to the widest name that actually has hooks.
	o.Hooks.Width = hookWidth(p)

	started := time.Now()

	if failed := runBefore(ctx, p, o); len(failed) > 0 {
		return fmt.Errorf(
			"%d service(s) failed their before hook after %s; nothing was deployed:\n  - %s",
			len(failed), time.Since(started).Round(time.Second),
			strings.Join(failed, "\n  - "))
	}

	// Warn when the worker limit will actually bite. Without this the per-target
	// timings look fine while the run takes twice as long, because half the
	// services spent that time waiting for a slot.
	if n := countWork(p); n > o.Concurrency {
		fmt.Fprintf(o.Out, "  %d services, %d at a time — some will wait for a slot\n",
			n, o.Concurrency)
	}

	var (
		mu     sync.Mutex
		failed []string
		wg     sync.WaitGroup
		sem    = make(chan struct{}, o.Concurrency)
	)

	for _, cp := range p.Services {
		if !cp.HasWork() {
			continue
		}

		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := applyService(ctx, cp, o); err != nil {
				mu.Lock()
				failed = append(failed, fmt.Sprintf("%s: %v", cp.Service.Name, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("%d service(s) failed after %s:\n  - %s",
			len(failed), time.Since(started).Round(time.Second),
			strings.Join(failed, "\n  - "))
	}

	// The total, which is the number nobody can work out from the per-target
	// times: those are measured from when a target got a slot, not from when
	// the run began.
	fmt.Fprintf(o.Out, "\ndone in %s\n", time.Since(started).Round(time.Second))
	return nil
}

func countWork(p *Plan) int {
	var n int
	for _, cp := range p.Services {
		if cp.HasWork() {
			n++
		}
	}
	return n
}

// runBefore runs every before hook and returns the services whose hook failed.
//
// The gate is the release, not the service. A before hook is the one failure
// that is knowable to have written nothing — it runs ahead of every API call —
// so a schema check that goes red means the release is already dead, and
// rolling the other services out anyway buys nothing but an environment that
// is half a version ahead. In a pipeline it also buys twenty more minutes of
// waiting for a run that was already lost at second sixteen.
//
// Every hook runs before any failure is reported: three services with a broken
// schema should produce three messages, not one per run.
func runBefore(ctx context.Context, p *Plan, o Options) []string {
	var (
		mu     sync.Mutex
		failed []string
		wg     sync.WaitGroup
		sem    = make(chan struct{}, o.Concurrency)
	)

	for _, cp := range p.Services {
		if !cp.HasWork() || len(cp.Service.Before) == 0 {
			continue
		}

		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := o.Hooks.Run(ctx, cp.Service.Name, "before", cp.Service.Before, hookVars(cp)); err != nil {
				mu.Lock()
				failed = append(failed, fmt.Sprintf("%s: %v", cp.Service.Name, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	sort.Strings(failed)
	return failed
}

// hookWidth sizes the [service] tag so hook output from different services
// lines up, the same way the plan aligns its target labels. Only services that
// actually have hooks count: a name that never prints must not indent the ones
// that do.
func hookWidth(p *Plan) int {
	var width int
	for _, cp := range p.Services {
		if !cp.HasWork() {
			continue
		}
		if len(cp.Service.Before) == 0 && len(cp.Service.After) == 0 {
			continue
		}
		width = max(width, len(cp.Service.Name)+2)
	}
	return width
}

// hookVars are the substitutions a hook is given, for either phase.
func hookVars(cp *ServicePlan) hooks.Vars {
	return hooks.Vars{
		"version": cp.Service.Version,
		"name":    cp.Service.Name,
		"env":     cp.Env,
	}
}

func applyService(ctx context.Context, cp *ServicePlan, o Options) error {
	c := cp.Service

	var (
		mu        sync.Mutex
		succeeded []*target.Change
		errs      []string
		wg        sync.WaitGroup
	)

	for _, ch := range cp.Changes {
		wg.Go(func() {
			// The plan has already been printed, so repeating it here would just
			// be a worse second copy. What is worth reporting is when a target
			// finishes and how long it took — the number that tells you whether
			// a deploy is slow because of the tool or because of a readiness
			// probe that waits a minute before its first check.
			started := time.Now()

			if err := o.Driver.Apply(ctx, ch); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("%s/%s: %v", ch.Target.Type, ch.Target.Name, err))
				mu.Unlock()
				fmt.Fprintf(o.Out, "  %-*s failed after %s\n", o.Width,
					ch.Target.Label(),
					time.Since(started).Round(time.Second))
				return
			}
			mu.Lock()
			succeeded = append(succeeded, ch)
			mu.Unlock()

			fmt.Fprintf(o.Out, "  %-*s %s in %s\n", o.Width,
				ch.Target.Label(),
				ch.ToVersion, time.Since(started).Round(time.Second))
		})
	}
	wg.Wait()

	if len(errs) > 0 {
		errs = append(errs, revert(ctx, succeeded, o)...)
		sort.Strings(errs)
		return fmt.Errorf("\n      %s", strings.Join(errs, "\n      "))
	}

	// after only runs on success, and a failure here does not roll anything
	// back: the deploy worked, and removing a working version because a
	// registration call failed is worse than the missing registration.
	if err := o.Hooks.Run(ctx, c.Name, "after", c.After, hookVars(cp)); err != nil {
		return err
	}
	return nil
}

// revert puts back the targets that succeeded while a sibling failed, and
// returns what could not be put back. The service is the boundary because its
// targets share one image.
//
// Concurrently, for the same reason the rollout is: these are independent
// writes that each wait on a cloud. One service with five targets was spending
// a minute reverting them one after another, on top of a deploy that had
// already failed.
func revert(ctx context.Context, changes []*target.Change, o Options) []string {
	var (
		mu   sync.Mutex
		errs []string
		wg   sync.WaitGroup
	)

	for _, ch := range changes {
		fmt.Fprintf(o.Out, "  %-*s reverting to %s\n", o.Width,
			ch.Target.Label(), orNone(ch.FromVersion))

		wg.Go(func() {
			if err := o.Driver.Revert(ctx, ch); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("%s/%s: revert failed: %v",
					ch.Target.Type, ch.Target.Name, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	return errs
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
