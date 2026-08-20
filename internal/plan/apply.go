package plan

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evolve-platform/evolve-deploy/internal/console"
	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// waitNotice is how long a target may go quiet before the log says it is still
// there.
//
// A rollout is a write and then two minutes of polling, and the polling prints
// nothing. In a pipeline log that reads as a hang, and someone cancels a deploy
// that was going fine. Ten seconds is what terraform settled on for the same
// problem, and it is short enough that the first notice arrives while whoever
// started the release is still watching.
const waitNotice = 10 * time.Second

// defaultConcurrency is set above the service count of any repository this is
// aimed at, so the limit does not quietly serialise a deploy. Nine services
// against a limit of eight cost a full extra wave before anyone noticed.
const defaultConcurrency = 16

// Options configure an apply.
type Options struct {
	Driver target.Driver
	Hooks  *hooks.Runner
	// Log is where every line of the run is written, tagged and aligned.
	Log *console.Log
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
	// WaitNotice overrides how long a target may go quiet before the log says it
	// is still being waited on. Zero means waitNotice.
	WaitNotice time.Duration
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
	if o.WaitNotice <= 0 {
		o.WaitNotice = waitNotice
	}

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
		o.Log.Plain("%d services, %d at a time — some will wait for a slot",
			n, o.Concurrency)
	}

	// One record per service that has work, built before anything starts so
	// that the map is only ever read from the goroutines below.
	runs := make(map[string]*serviceRun, len(p.Services))
	for _, cp := range p.Services {
		if cp.HasWork() {
			runs[cp.Service.Name] = &serviceRun{plan: cp, done: make(chan struct{})}
		}
	}

	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, o.Concurrency)
	)

	for _, r := range runs {
		wg.Go(func() {
			defer close(r.done)

			if blocker := awaitDeps(ctx, r, runs); blocker != "" {
				r.blocker = blocker
				o.Log.Note(r.plan.Service.Name, "skipped, %s did not deploy", blocker)
				return
			}

			// The slot is claimed only once this service can actually run.
			// Taking one while still waiting on a dependency would let a chain
			// of services fill every slot with goroutines that cannot proceed,
			// and the release would sit there until the deadline.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				r.err = ctx.Err()
				return
			}
			defer func() { <-sem }()

			r.err = applyService(ctx, r.plan, o)
		})
	}
	wg.Wait()

	var failed, skipped []string
	for _, name := range slices.Sorted(maps.Keys(runs)) {
		switch r := runs[name]; {
		case r.err != nil:
			failed = append(failed, fmt.Sprintf("%s: %v", name, r.err))
		case r.blocker != "":
			skipped = append(skipped, fmt.Sprintf("%s, waiting on %s", name, r.blocker))
		}
	}

	if len(failed) > 0 {
		msg := fmt.Sprintf("%d service(s) failed after %s:\n  - %s",
			len(failed), time.Since(started).Round(time.Second),
			strings.Join(failed, "\n  - "))
		// Named separately from the failures, because a service that never ran
		// has nothing wrong with it and counting it as a failure sends whoever
		// reads this looking in the wrong place.
		if len(skipped) > 0 {
			msg += fmt.Sprintf("\n%d service(s) were not deployed at all:\n  - %s",
				len(skipped), strings.Join(skipped, "\n  - "))
		}
		return errors.New(msg)
	}

	// The total, which is the number nobody can work out from the per-target
	// times: those are measured from when a target got a slot, not from when
	// the run began.
	o.Log.Blank()
	o.Log.Plain("done in %s", time.Since(started).Round(time.Second))
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

// serviceRun tracks one service through the release.
//
// err and blocker are written by that service's own goroutine before its done
// channel closes, so anything that has waited on done may read them without a
// lock.
type serviceRun struct {
	plan *ServicePlan
	done chan struct{}

	// err is a real failure: the service ran and something went wrong.
	err error
	// blocker names the dependency that stopped this service from running at
	// all. Nothing was written for it, so it is not a failure of its own.
	blocker string
}

// awaitDeps blocks until every service this one depends on has finished, and
// returns the name of one that did not make it.
//
// A dependency that is not part of this run is already satisfied and is not
// waited for. That is what makes depends_on usable in CI: the pipeline deploys
// only what it rebuilt, so the backend a frontend names is often simply not
// there — and demanding it be present would fail every release that touched
// one service.
func awaitDeps(ctx context.Context, r *serviceRun, runs map[string]*serviceRun) string {
	for _, name := range r.plan.Service.DependsOn {
		dep, ok := runs[name]
		if !ok {
			continue
		}

		select {
		case <-dep.done:
		case <-ctx.Done():
			return name
		}

		if dep.err != nil || dep.blocker != "" {
			return name
		}
	}
	return ""
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

			// The driver is handed a context it can report through, and a
			// progress line goes out for as long as it takes. Both stop the
			// moment it returns.
			tctx, stop := watch(ctx, o, c.Name, ch, started)
			err := o.Driver.Apply(tctx, ch)
			stop()

			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("%s/%s: %v", ch.Target.Type, ch.Target.Name, err))
				mu.Unlock()
				o.Log.Line(c.Name, ch.Target.Label(), "failed after %s",
					time.Since(started).Round(time.Second))
				return
			}
			mu.Lock()
			succeeded = append(succeeded, ch)
			mu.Unlock()

			o.Log.Line(c.Name, ch.Target.Label(), "%s in %s",
				ch.ToVersion, time.Since(started).Round(time.Second))
		})
	}
	wg.Wait()

	if len(errs) > 0 {
		errs = append(errs, revert(ctx, cp, succeeded, o)...)
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
func revert(ctx context.Context, cp *ServicePlan, changes []*target.Change, o Options) []string {
	var (
		mu   sync.Mutex
		errs []string
		wg   sync.WaitGroup
	)

	for _, ch := range changes {
		o.Log.Line(cp.Service.Name, ch.Target.Label(), "reverting to %s",
			orNone(ch.FromVersion))

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

// watch reports that a target is still being waited on, every waitNotice until
// the returned function is called.
//
// What it is waiting for comes from the driver, which is the only thing that
// knows: the same ten-second silence can be an image being pulled, a probe that
// has not passed, or an ARM operation that has not been acknowledged. Nothing
// reported yet reads as the platform in general, which is true and is still
// better than no line at all.
func watch(
	ctx context.Context, o Options, service string, ch *target.Change, started time.Time,
) (context.Context, func()) {
	var status atomic.Pointer[string]
	ctx = target.WithStatus(ctx, func(note string) { status.Store(&note) })

	done := make(chan struct{})
	// Closed last, so stopping waits for a line that is already being written.
	// Without that, a notice can land under the target's own result and read as
	// a target that is somehow both finished and still going.
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		t := time.NewTicker(o.WaitNotice)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				waiting := "the platform"
				if note := status.Load(); note != nil {
					waiting = *note
				}
				o.Log.Line(service, ch.Target.Label(), "still waiting on %s, %s",
					waiting, time.Since(started).Round(time.Second))
			}
		}
	}()

	return ctx, func() {
		close(done)
		<-stopped
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
