package plan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evolve-platform/evolve-deploy/internal/config"
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

	// One record per unit of work, built before anything starts so that the map
	// is only ever read from the goroutines below.
	//
	// Every blue-green service is one unit, not one each: the side belongs to the
	// environment, so the whole of it is staged, gated and switched together.
	runs := make(map[string]*serviceRun, len(p.Services)+1)
	var release []*ServicePlan
	for _, cp := range p.Services {
		if !cp.HasWork() {
			continue
		}
		if cp.BlueGreen() {
			release = append(release, cp)
			continue
		}
		runs[cp.Service.Name] = &serviceRun{
			name: cp.Service.Name,
			deps: cp.Service.DependsOn,
			done: make(chan struct{}),
			run:  func(ctx context.Context) error { return applyService(ctx, cp, o) },
		}
	}
	if len(release) > 0 {
		// Its dependencies are every dependency the staged services declare.
		// They can only name a `direct` service — an edge between two blue-green
		// services is refused while reading the config, because there is nothing
		// left to order once the whole side goes at once.
		var deps []string
		for _, cp := range release {
			deps = append(deps, cp.Service.DependsOn...)
		}
		runs[releaseNode] = &serviceRun{
			name: releaseNode,
			deps: deps,
			done: make(chan struct{}),
			run:  func(ctx context.Context) error { return applyRelease(ctx, p, release, o) },
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
				fmt.Fprintf(o.Out, "  %-*s skipped, %s did not deploy\n",
					o.Width, r.name, blocker)
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

			r.err = r.run(ctx)
		})
	}
	wg.Wait()

	var (
		failed, skipped []string
		relErr          *releaseError
	)
	for _, name := range slices.Sorted(maps.Keys(runs)) {
		switch r := runs[name]; {
		case r.err != nil:
			// The staged release is not one service among several, and reporting
			// it as one read as "1 service(s) failed" above a list of three
			// container apps. It is one phase of one side, so it says so itself.
			var rel *releaseError
			if errors.As(r.err, &rel) {
				relErr = rel
				continue
			}
			failed = append(failed, fmt.Sprintf("%s: %v", name, r.err))
		case r.blocker != "":
			skipped = append(skipped, fmt.Sprintf("%s, waiting on %s", name, r.blocker))
		}
	}

	if len(failed) > 0 || relErr != nil {
		elapsed := time.Since(started).Round(time.Second)

		var parts []string
		if relErr != nil {
			parts = append(parts, relErr.describe(elapsed))
		}
		if len(failed) > 0 {
			parts = append(parts, fmt.Sprintf("%d service(s) failed after %s:\n  - %s",
				len(failed), elapsed, strings.Join(failed, "\n  - ")))
		}
		// Named separately from the failures, because a service that never ran
		// has nothing wrong with it and counting it as a failure sends whoever
		// reads this looking in the wrong place.
		if len(skipped) > 0 {
			parts = append(parts, fmt.Sprintf("%d service(s) were not deployed at all:\n  - %s",
				len(skipped), strings.Join(skipped, "\n  - ")))
		}
		return errors.New(strings.Join(parts, "\n"))
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

// serviceRun tracks one service through the release.
//
// err and blocker are written by that service's own goroutine before its done
// channel closes, so anything that has waited on done may read them without a
// lock.
type serviceRun struct {
	// name is what failures are reported under: a service, or the whole staged
	// release.
	name string
	// deps are the names this unit waits for, if they are part of this run.
	deps []string
	run  func(context.Context) error
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
	for _, name := range r.deps {
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

			if err := o.Hooks.RunWith(ctx, cp.Service.Name, "before",
				cp.Service.Before, cp.HookVars().Data(), cp.hookFuncs); err != nil {
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
	if err := o.Hooks.RunWith(ctx, c.Name, "after",
		c.After, cp.HookVars().Data(), cp.hookFuncs); err != nil {
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

// applyBlueGreen releases one service a side at a time.
//
// The shape is the point: everything before the switch is reversible without a
// user noticing, so a release that fails its gate is a non-event rather than an
// outage. Staging is where the time goes, the smoke test is the gate, and the
// switch is one write.
// releaseNode is the name the staged release is reported under. Not a service
// name, and it cannot collide with one: the config reserves it.
const releaseNode = config.ReleaseName

// releaseError is the failure of the staged release as a whole.
//
// It reads as a paragraph rather than as one entry in a list of services,
// because that is what it is: one side, one phase of it, and a set of apps that
// went wrong together. What that paragraph has to end on is what is serving now
// — a release that failed after ten minutes of staging is read by someone who
// wants to know whether the environment is still up before they want to know
// why it is not — so the sentence is carried separately and printed last. The
// elapsed time is left to Apply, which is the only place that knows it.
type releaseError struct {
	// phase is what the release was doing, and reads after "while".
	phase string
	// state is what is serving now, as its own sentence.
	state string
	// errs is one line per thing that went wrong.
	errs []string
}

func (e *releaseError) Error() string { return e.describe(0) }

func (e *releaseError) describe(after time.Duration) string {
	head := "the release failed"
	if e.phase != "" {
		head += " while " + e.phase
	}
	if after > 0 {
		head += " after " + after.String()
	}
	return fmt.Sprintf("%s:\n  - %s\n%s", head, strings.Join(e.errs, "\n  - "), e.state)
}

// bgTarget is one staged target and the service it belongs to.
//
// The phases run over the whole release while hooks and smoke commands stay a
// property of a service, so both have to travel together.
type bgTarget struct {
	ch *target.Change
	cp *ServicePlan
}

func changes(items []bgTarget) []*target.Change {
	out := make([]*target.Change, 0, len(items))
	for _, it := range items {
		out = append(out, it.ch)
	}
	return out
}

// applyRelease stages every blue-green service, gates the lot, and switches it
// in one go.
//
// The phases are release-wide, not per service, and that is the whole design:
// the side belongs to the environment, so a half-staged side is not a stack and
// testing one app of it proves nothing about the release. Stage everything,
// check it, then move the traffic — and if any part of it fails, none of it ever
// served.
//
// It follows that a service with nothing to change is staged too. Its revision
// carries the same image as the one already serving; what it gives the release is
// a side that is complete, which is what the smoke commands are pointed at.
func applyRelease(ctx context.Context, p *Plan, plans []*ServicePlan, o Options) error {
	rollout, ok := o.Driver.(target.Rollout)
	if !ok {
		// Refused during planning; here it would mean the plan and the apply
		// disagree about the driver, which is a bug rather than a config error.
		return fmt.Errorf("%s cannot move traffic", o.Driver.Name())
	}

	// Riders are the targets with no ingress — a container app job, a lambda
	// beside a service. They share an image with the thing being released, so
	// they move with the traffic and not before it: writing them early points
	// the 03:00 cron at code the API is not serving yet.
	var routable, riders []bgTarget
	for _, cp := range plans {
		for _, ch := range cp.Changes {
			if ch.Sides != nil {
				routable = append(routable, bgTarget{ch, cp})
			} else {
				riders = append(riders, bgTarget{ch, cp})
			}
		}
	}

	// checkOneSide has already refused a release whose apps disagree, so these
	// two names describe every target in it.
	side, serving := p.Side()

	staged, errs := stageAll(ctx, rollout, routable, o)
	phase := "staging " + side
	if len(errs) == 0 {
		if errs = smokeRelease(ctx, p, routable, staged, o); len(errs) > 0 {
			phase = "the smoke test"
		}
	}
	if len(errs) > 0 {
		// Nothing has served from any of this, so putting it back is free.
		undone := abandonAll(ctx, rollout, changes(routable), o)
		return &releaseError{
			phase: phase,
			state: nothingServed(side, serving, len(undone) > 0),
			errs:  sorted(errs, undone),
		}
	}

	if errs := switchAll(ctx, rollout, routable, riders, o); len(errs) > 0 {
		undone := abandonAll(ctx, rollout, changes(routable), o)
		undone = append(undone, revert(ctx, changes(riders), o)...)
		return &releaseError{
			phase: "switching to " + side,
			state: putBack(serving, len(undone) > 0),
			errs:  sorted(errs, undone),
		}
	}

	// A failure from here is a warning: the traffic is on the new version and
	// the deploy worked. Removing a working version over a cleanup call that
	// returned 500 is worse than the leftover it would tidy.
	for _, it := range routable {
		if err := rollout.Settle(ctx, it.ch); err != nil {
			fmt.Fprintf(o.Out, "  %-*s deployed, but the cleanup failed: %v\n",
				o.Width, it.ch.Target.Label(), err)
			fmt.Fprintf(o.Out, "  %-*s (an old revision is still running and still costs money; "+
				"the next release tidies it)\n", o.Width, "")
		}
	}

	for _, it := range routable {
		// After a blue-green deploy the question is not only whether it worked
		// but what it falls back to, and this is the only place that knows.
		// Whether that fallback is warm or stopped is part of the answer: it is
		// the difference between a rollback that is one write and one that
		// starts a container first.
		fmt.Fprintf(o.Out, "  %-*s %s serves %s, rollback is %s %s (%s)\n", o.Width,
			it.ch.Target.Label(), it.ch.Sides.Idle.Label, it.ch.ToVersion,
			it.ch.Sides.Active.Label, orNone(it.ch.FromVersion), it.ch.Fallback)
	}

	// The after hooks are still per service and still run only once its own
	// targets are all serving — which, now that the switch is one phase, is the
	// same moment for everything. A failure here fails the release without
	// rolling anything back, as it always has.
	var failed []string
	for _, cp := range plans {
		c := cp.Service
		if len(c.After) == 0 {
			continue
		}
		if err := o.Hooks.RunWith(ctx, c.Name, "after",
			c.After, cp.HookVars().Data(), cp.hookFuncs); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", c.Name, err))
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		return &releaseError{
			phase: "running the after hooks",
			state: fmt.Sprintf(
				"The deploy itself worked: %s is serving and nothing was rolled back", side),
			errs: failed,
		}
	}
	return nil
}

// nothingServed is the sentence for a release that failed before the traffic
// moved. It is the first thing worth knowing, and on a release that failed after
// ten minutes of staging it is usually the only thing worth knowing.
func nothingServed(side, serving string, kept bool) string {
	if kept {
		// The reassurance would be a lie, and the list below says which app it
		// would be a lie about.
		return "Some of the staged side could not be put back — check the traffic " +
			"on the targets below before retrying"
	}
	return fmt.Sprintf(
		"No traffic moved: %s still serves the version it served before, "+
			"and the %s revisions were switched off", serving, side)
}

// putBack is the same sentence for a switch that failed, where some of the
// traffic did move and was moved back.
func putBack(serving string, kept bool) string {
	if kept {
		return "The traffic could not be put back everywhere — check it on the " +
			"targets below before retrying"
	}
	return fmt.Sprintf("The traffic is back on %s, which serves the version "+
		"it served before", serving)
}

// sorted is the failures of a phase followed by whatever the cleanup after it
// could not undo, in one list. Sorted so that a release of fourteen services
// reports them in the same order twice.
func sorted(errs, cleanup []string) []string {
	out := append(errs, cleanup...)
	sort.Strings(out)
	return out
}

// stageAll creates every new revision at once. They carry no traffic, so there
// is nothing to serialise and staging is the slow part of the release.
func stageAll(
	ctx context.Context,
	r target.Rollout,
	items []bgTarget,
	o Options,
) (map[*target.Change]*target.Staged, []string) {
	var (
		mu     sync.Mutex
		staged = map[*target.Change]*target.Staged{}
		errs   []string
		wg     sync.WaitGroup
	)

	for _, it := range items {
		ch := it.ch
		wg.Go(func() {
			started := time.Now()
			got, err := r.Stage(ctx, ch)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", ch.Target.Label(), err))
				fmt.Fprintf(o.Out, "  %-*s failed to stage after %s\n",
					o.Width, ch.Target.Label(), time.Since(started).Round(time.Second))
				return
			}
			staged[ch] = got
			fmt.Fprintf(o.Out, "  %-*s staged %s in %s\n", o.Width,
				ch.Target.Label(), got.Label, time.Since(started).Round(time.Second))
		})
	}
	wg.Wait()

	sort.Strings(errs)
	return staged, errs
}

// smokeRelease runs the gate once, over the whole staged side.
//
// Once, not once per target, because the side belongs to the environment: a
// request through the staged router reaches the staged subgraphs, and that is
// the thing worth checking. Running the same suite per service would run it
// fourteen times and still only ever prove something about one app at a time.
//
// There is no {{.url}} for the same reason — a release has no single address —
// so a command names what it wants: {{url "site"}}. A function rather than a
// field because most service names have a hyphen in them and a template field
// has to be an identifier.
func smokeRelease(
	ctx context.Context,
	p *Plan,
	items []bgTarget,
	staged map[*target.Change]*target.Staged,
	o Options,
) []string {
	smoke := p.SmokeHooks()
	if len(smoke) == 0 {
		return nil
	}

	funcs := SmokeFuncs(stagedLookup(items, staged))
	started := time.Now()

	if err := o.Hooks.RunWith(
		ctx, releaseNode, "smoke", smoke, p.SmokeVars(), funcs); err != nil {
		fmt.Fprintf(o.Out, "  %-*s smoke failed after %s, no traffic was moved\n",
			o.Width, releaseNode, time.Since(started).Round(time.Second))
		return []string{err.Error()}
	}

	fmt.Fprintf(o.Out, "  %-*s smoke passed in %s\n", o.Width,
		releaseNode, time.Since(started).Round(time.Second))
	return nil
}

// stagedLookup resolves a name in a smoke command to what was actually staged.
//
// Both a service name and a target label work. A name that staged nothing is an
// error rather than an empty string: a smoke test quietly pointed at "" would
// pass, and a gate that passes when it cannot reach anything is worse than no
// gate. The same goes for an address the platform does not have — a Cloud Run
// revision has no URL of its own, and falling back to the label's would answer
// a different question than the one that was asked.
func stagedLookup(
	items []bgTarget,
	staged map[*target.Change]*target.Staged,
) lookupFunc {
	type addresses struct {
		got *target.Staged
		ch  *target.Change
	}
	byName := map[string]addresses{}
	perService := map[string]int{}
	for _, it := range items {
		got, ok := staged[it.ch]
		if !ok {
			continue
		}
		byName[it.ch.Target.Label()] = addresses{got, it.ch}
		perService[it.cp.Service.Name]++
		byName[it.cp.Service.Name] = addresses{got, it.ch}
	}
	// A service with two staged sides cannot be named by its own name, because
	// that name cannot mean either of them.
	for name, n := range perService {
		if n > 1 {
			delete(byName, name)
		}
	}

	return func(kind, name string) (string, error) {
		at, ok := byName[name]
		if !ok {
			known := slices.Sorted(maps.Keys(byName))
			return "", fmt.Errorf("%q staged nothing in this release — staged: %s",
				name, strings.Join(known, ", "))
		}
		switch kind {
		case urlStage:
			if at.got.URL == "" {
				return "", fmt.Errorf("%q has no reachable address for its staged side", name)
			}
			return at.got.URL, nil
		case urlRevision:
			if at.got.RevisionURL == "" {
				return "", fmt.Errorf(
					"%q staged a revision with no address of its own — only Container Apps "+
						"gives every revision one. Use {{url_stage %q}}, which reaches the "+
						"same revision through the label this release just attached",
					name, name)
			}
			return at.got.RevisionURL, nil
		case urlPublic:
			if at.ch.PublicURL == "" {
				return "", fmt.Errorf("%q has no address of its own", name)
			}
			return at.ch.PublicURL, nil
		case "label":
			return at.got.Label, nil
		case "revision":
			return at.got.Revision, nil
		}
		return "", fmt.Errorf("unknown smoke function %q", kind)
	}
}

// switchAll moves the traffic and writes the riders in the same step.
//
// The riders go with it rather than before it: a job shares its image with the
// service beside it, and updating it while the old version still serves is the
// mixed state this whole mechanism exists to avoid.
func switchAll(
	ctx context.Context,
	r target.Rollout,
	routable, riders []bgTarget,
	o Options,
) []string {
	var (
		mu   sync.Mutex
		errs []string
		wg   sync.WaitGroup
	)
	fail := func(format string, args ...any) {
		mu.Lock()
		errs = append(errs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	for _, it := range routable {
		ch := it.ch
		wg.Go(func() {
			if err := r.Switch(ctx, ch); err != nil {
				fail("%s: switching to %s: %v", ch.Target.Label(), ch.Sides.Idle.Label, err)
			}
		})
	}
	for _, it := range riders {
		ch := it.ch
		wg.Go(func() {
			if err := o.Driver.Apply(ctx, ch); err != nil {
				fail("%s: %v", ch.Target.Label(), err)
			}
		})
	}
	wg.Wait()

	sort.Strings(errs)
	return errs
}

// abandonAll puts the traffic back and switches off the revisions that never
// served anything.
//
// It says so as it goes. A release that fails leaves the question of what is
// running now, and a silent cleanup means reading the failure and then going to
// the portal to find out whether a staged side is still up and still being
// charged for. The answer belongs in the output that raised the question.
func abandonAll(
	ctx context.Context,
	r target.Rollout,
	changes []*target.Change,
	o Options,
) []string {
	var (
		mu   sync.Mutex
		errs []string
		wg   sync.WaitGroup
	)

	for _, ch := range changes {
		fmt.Fprintf(o.Out, "  %-*s discarding %s, %s keeps serving %s\n", o.Width,
			ch.Target.Label(), ch.Sides.Idle.Label, ch.Sides.Active.Label,
			orNone(ch.FromVersion))

		wg.Go(func() {
			if err := r.Abandon(ctx, ch); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("%s: could not put the traffic back: %v",
					ch.Target.Label(), err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	sort.Strings(errs)
	return errs
}
