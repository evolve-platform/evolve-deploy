// Package plan turns a config file into a set of changes, without touching
// anything.
//
// Every failure that can be found by reading is found here: unknown reference
// schemes, references that do not exist, artifacts that were never built,
// secrets that would have to be read under a deny policy. Only when the whole
// plan is clean does anything get deployed.
package plan

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/template"

	"golang.org/x/sync/errgroup"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// planConcurrency bounds how many services are read at once. High enough that
// planning is not the slow part, low enough not to trip an API rate limit on a
// repository with dozens of services.
const planConcurrency = 16

// Plan is the full set of work for one apply, grouped by service so that
// hooks and rollback have the right boundary.
type Plan struct {
	File     *config.File
	Services []*ServicePlan

	// vars are the --var values. Kept on the plan as well as on each service
	// because the smoke commands belong to the release and have no service to
	// take them from.
	vars map[string]string
}

// ServicePlan is one service's work. Changes holds only the targets that
// need something; Unchanged names the ones already at the desired state.
type ServicePlan struct {
	Service   *config.Service
	Changes   []*target.Change
	Unchanged []*config.Target
	// Env is the environment name, carried here so hooks can be given {{.env}}
	// without a back-pointer to the file.
	Env string

	// Side is the label this release is going to, and Previous the one it is
	// coming from. Both empty unless this service releases a side at a time.
	//
	// They live on the service rather than the target because hooks do: a
	// service whose routable targets disagree about which side they are going to
	// is refused while planning, so one value here is always the whole truth.
	Side     string
	Previous string

	// Vars are the --var values, kept here so hook variables are assembled in
	// one place.
	Vars map[string]string
}

// BlueGreen reports whether this service stages a side and switches to it.
func (c *ServicePlan) BlueGreen() bool { return c.Side != "" }

// EnvRemovals lists every environment variable this plan would delete, as
// "target: NAME".
//
// Declaring an environment in the config means owning all of it, and the first
// time someone does that for a service Terraform has been filling, the natural
// mistake is to list the one variable they care about — which asks the tool to
// drop the other thirty, secrets included. That is a service that stops
// starting, so apply refuses unless it is confirmed.
func (p *Plan) EnvRemovals() []string {
	var out []string
	for _, cp := range p.Services {
		for _, ch := range cp.Changes {
			for _, name := range ch.EnvRemoved {
				out = append(out, fmt.Sprintf("%s: %s", ch.Target.Label(), name))
			}
		}
	}
	sort.Strings(out)
	return out
}

// SmokeCommands gate the release. They run once, after everything has staged
// and before anything switches.
//
// Per release rather than per service, because that is what the useful test is.
// One suite run once per service runs it fourteen times for one release, and the
// request worth making goes through a router and touches several services at
// once — so it belongs to none of them. A check of a single revision has not
// been lost: it is a line in the same set, `curl -fsS {{url "purchase"}}/healthz`.
func (p *Plan) SmokeCommands() []string {
	if p.File.Strategy == nil || !p.Staging() {
		return nil
	}
	return p.File.Strategy.Smoke
}

// Staging reports whether this release stages a side at all, which is what
// there would be to smoke test.
func (p *Plan) Staging() bool {
	for _, cp := range p.Services {
		for _, ch := range cp.Changes {
			if ch.Sides != nil {
				return true
			}
		}
	}
	return false
}

// Side is the side this release stages on, and Previous the one still serving.
//
// One value for the whole release rather than one per service: checkOneSide has
// already refused anything else, because the side belongs to the environment.
func (p *Plan) Side() (side, previous string) {
	for _, cp := range p.Services {
		for _, ch := range cp.Changes {
			if ch.Sides != nil {
				return ch.Sides.Idle.Label, ch.Sides.Active.Label
			}
		}
	}
	return "", ""
}

// StagedNames is every name a smoke command may ask for: each service that
// stages a side, and each routable target.
//
// Both, because a service is the obvious way to name one and a target is the
// unambiguous way — a service with two container apps stages two sides, and its
// own name cannot mean either.
func (p *Plan) StagedNames() []string {
	var names []string
	for _, cp := range p.Services {
		var routable int
		for _, ch := range cp.Changes {
			if ch.Sides == nil {
				continue
			}
			routable++
			names = append(names, ch.Target.Label())
		}
		if routable == 1 {
			names = append(names, cp.Service.Name)
		}
	}
	sort.Strings(names)
	return names
}

// SmokeVars are the values a smoke command is given besides its functions.
func (p *Plan) SmokeVars() map[string]any {
	side, previous := p.Side()
	out := map[string]any{"env": p.File.Env}
	// The tool's own names last, so a --var cannot shadow the side the release
	// is actually going to.
	for k, v := range p.vars {
		out[k] = v
	}
	out["label"] = side
	out["previous_label"] = previous
	return out
}

// SmokeFuncs builds the functions a smoke command calls to name a service.
//
// A function rather than a field because a Go template field has to be an
// identifier, and {{.catalog-commercetools.url}} does not even parse. One form
// that always works beats two where one of them is a trap.
func SmokeFuncs(lookup func(kind, name string) (string, error)) template.FuncMap {
	fn := func(kind string) func(string) (string, error) {
		return func(name string) (string, error) { return lookup(kind, name) }
	}
	return template.FuncMap{
		"url":      fn("url"),
		"label":    fn("label"),
		"revision": fn("revision"),
	}
}

// checkSmoke renders the release's smoke commands without running them.
//
// No url exists until something is staged, so the functions return nothing
// here. What is checked is that the commands parse and that every name they ask
// for is one this release will actually stage — both of which a typo gets wrong,
// and both far better found now than after a staging phase that took minutes.
func (p *Plan) checkSmoke() error {
	commands := p.SmokeCommands()
	if len(commands) == 0 {
		return nil
	}

	names := p.StagedNames()
	funcs := SmokeFuncs(func(_, name string) (string, error) {
		if !slices.Contains(names, name) {
			return "", fmt.Errorf(
				"%q does not stage a side in this release — it stages: %s",
				name, strings.Join(names, ", "))
		}
		return "", nil
	})

	if err := hooks.ValidateWith(commands, p.SmokeVars(), funcs); err != nil {
		return fmt.Errorf("strategy.smoke: %w", err)
	}
	return nil
}

// HasWork reports whether anything would be deployed. Hooks do not run for a
// service with no work — no deploy, no schema publish.
func (c *ServicePlan) HasWork() bool { return len(c.Changes) > 0 }

// Empty reports whether the whole plan is a no-op, which is the normal outcome
// of running twice.
func (p *Plan) Empty() bool {
	for _, c := range p.Services {
		if c.HasWork() {
			return false
		}
	}
	return true
}

// Build resolves every reference and asks the driver what would change.
//
// Errors are collected rather than returned on the first failure: someone who
// mistyped three references should see three messages, not one per run.
func Build(
	ctx context.Context,
	f *config.File,
	d target.Driver,
	vars map[string]string,
) (*Plan, error) {
	if err := d.Verify(ctx); err != nil {
		return nil, err
	}

	names := f.ServiceNames()

	// Services are planned concurrently. Every call here is read-only, so the
	// only cost of doing them one at a time is latency — and that adds up: a
	// round trip per target, times a repository with a dozen services, is
	// seconds of waiting before anything has been decided.
	//
	// Results are written to fixed positions so the plan comes out in the same
	// order every run, whatever order the answers arrive in.
	plans := make([]*ServicePlan, len(names))

	var (
		mu       sync.Mutex
		problems []string
	)
	report := func(msgs ...string) {
		mu.Lock()
		problems = append(problems, msgs...)
		mu.Unlock()
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(planConcurrency)

	for i, name := range names {
		g.Go(func() error {
			cp, errs := planService(gctx, f, d, f.Services[name], vars)
			if len(errs) > 0 {
				report(errs...)
				return nil
			}
			plans[i] = cp
			return nil
		})
	}
	_ = g.Wait()

	p := &Plan{File: f, vars: vars}
	for _, cp := range plans {
		if cp != nil {
			p.Services = append(p.Services, cp)
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("plan failed, nothing was deployed:\n  - %s",
			strings.Join(problems, "\n  - "))
	}

	p.settleRelease()
	if msgs := p.checkOneSide(); len(msgs) > 0 {
		sort.Strings(msgs)
		return nil, fmt.Errorf("plan failed, nothing was deployed:\n  - %s",
			strings.Join(msgs, "\n  - "))
	}

	// Last, because the names a smoke command may use are only settled once
	// every service has been planned and the carried ones dropped.
	if err := p.checkSmoke(); err != nil {
		return nil, fmt.Errorf("plan failed, nothing was deployed:\n  - %w", err)
	}
	return p, nil
}

// settleRelease decides whether the carried targets are part of this release.
//
// A blue-green target with nothing to change is still planned, because the side
// it is staged on is a property of the environment: a side missing an app is not
// a stack and cannot be tested as one. But that is only worth a revision when
// the release is actually moving something — otherwise every run would stage
// every app, flip the environment and call it a deploy, and running `apply`
// twice would no longer be a no-op.
//
// So the question is asked once, for the whole release: does anything that
// carries traffic have a real change? If not, the carried targets go back to
// being unchanged. A release that only rewrites a job is not a reason to stage a
// whole side — the job rides along with an app that is already serving the
// version it shares.
//
// The side itself stays known either way, because hooks use it and a
// `{{.label}}` that works or fails depending on which of a service's targets
// happened to move is the worst kind of intermittent.
func (p *Plan) settleRelease() {
	for _, cp := range p.Services {
		for _, ch := range cp.Changes {
			if ch.Sides != nil && !ch.Carry {
				return
			}
		}
	}

	for _, cp := range p.Services {
		var keep []*target.Change
		for _, ch := range cp.Changes {
			if ch.Carry {
				cp.Unchanged = append(cp.Unchanged, ch.Target)
				continue
			}
			keep = append(keep, ch)
		}
		cp.Changes = keep
	}
}

// checkOneSide refuses a release whose apps do not agree on which side is idle.
//
// The side belongs to the environment, so "green" has to mean the same thing
// everywhere: the storefront staged on green reaches the router on green, which
// reaches the subgraphs on green. If one app is a side out of step, that chain
// points at whatever happens to be there — an older version, or nothing at all.
//
// It is a refusal rather than a repair because putting it right means choosing
// which version an app should be serving, and that is not the tool's call.
func (p *Plan) checkOneSide() []string {
	sides := map[string][]string{}
	for _, cp := range p.Services {
		for _, ch := range cp.Changes {
			if ch.Sides == nil {
				continue
			}
			sides[ch.Sides.Idle.Label] = append(sides[ch.Sides.Idle.Label], ch.Target.Label())
		}
	}
	if len(sides) < 2 {
		return nil
	}

	var lines []string
	for _, side := range slices.Sorted(maps.Keys(sides)) {
		targets := sides[side]
		sort.Strings(targets)
		lines = append(lines, fmt.Sprintf("      %-8s %s", side, strings.Join(targets, ", ")))
	}
	first := slices.Sorted(maps.Keys(sides))[0]
	return []string{fmt.Sprintf(
		"the environment is split across both sides, so there is no side to stage on:\n%s\n"+
			"    align it first with `evolve-deploy traffic <config> --to %s`",
		strings.Join(lines, "\n"), first)}
}

// planService resolves one service's targets and asks the driver what would
// change. Its targets are planned concurrently too, which matters for something
// like catalog-commercetools: one service and four jobs sharing an image.
func planService(
	ctx context.Context,
	f *config.File,
	d target.Driver,
	c *config.Service,
	vars map[string]string,
) (*ServicePlan, []string) {
	var problems []string

	cp := &ServicePlan{Service: c, Env: f.Env, Vars: vars}
	if msgs := checkStrategy(d, c); len(msgs) > 0 {
		return nil, msgs
	}
	desired := make([]*target.Desired, 0, len(c.Targets))

	for _, t := range c.Targets {
		slog.Debug("planning", "service", c.Name,
			"target", fmt.Sprintf("%s/%s", t.Type, t.Name), "version", c.Version)

		env, errs := resolveEnv(ctx, f, d, c, t)
		sideEnv, sideErrs := resolveSideEnv(ctx, f, d, t)
		errs = append(errs, sideErrs...)
		if len(errs) > 0 {
			problems = append(problems, errs...)
			continue
		}
		desired = append(desired, &target.Desired{
			Service:   c.Name,
			Version:   c.Version,
			Target:    t,
			Env:       env,
			ManageEnv: t.ManagesEnv,
			SideEnv:   sideEnv,
		})
	}
	if len(problems) > 0 {
		return nil, problems
	}

	changes := make([]*target.Change, len(desired))
	g, gctx := errgroup.WithContext(ctx)
	for i, want := range desired {
		g.Go(func() error {
			ch, err := d.Plan(gctx, want)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", want.Target.Type, want.Target.Name, err)
			}
			changes[i] = ch
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, []string{err.Error()}
	}

	for i, ch := range changes {
		t := desired[i].Target
		if ch == nil {
			slog.Debug("up to date", "target", fmt.Sprintf("%s/%s", t.Type, t.Name))
			cp.Unchanged = append(cp.Unchanged, t)
			continue
		}
		slog.Debug("change needed",
			"target", fmt.Sprintf("%s/%s", t.Type, t.Name),
			"from", ch.FromVersion, "to", ch.ToVersion, "reason", ch.Reason)

		// Asked once, here, so the plan and the apply say the same thing
		// without either of them knowing which cloud they are on.
		if ch.Sides != nil {
			if r, ok := d.(target.Rollout); ok {
				ch.Fallback = r.Fallback(t)
			}
		}
		cp.Changes = append(cp.Changes, ch)
	}

	if msgs := cp.settleSide(ctx, d); len(msgs) > 0 {
		return nil, msgs
	}
	if msgs := cp.checkHooks(); len(msgs) > 0 {
		return nil, msgs
	}
	return cp, nil
}

// checkStrategy refuses a blue-green service the driver cannot actually roll
// that way, before anything is read or written.
//
// The alternative would be falling back to a direct update, which is the one
// thing a deploy tool must not do quietly: the config asked for a staged
// release with a gate, and shipping straight to production instead is not a
// smaller version of that.
func checkStrategy(d target.Driver, c *config.Service) []string {
	if !c.Strategy.IsBlueGreen() {
		return nil
	}

	r, ok := d.(target.Rollout)
	if !ok {
		return []string{fmt.Sprintf(
			"services.%s: strategy blue-green is not implemented for %s yet",
			c.Name, d.Name())}
	}

	var routable, riders []string
	for _, t := range c.Targets {
		if r.Routable(t.Type) {
			routable = append(routable, t.Label())
		} else {
			riders = append(riders, t.Label())
		}
	}
	if len(routable) == 0 {
		return []string{fmt.Sprintf(
			"services.%s: strategy blue-green needs a target that carries traffic, "+
				"and %s cannot (there is no traffic to divide)",
			c.Name, strings.Join(riders, ", "))}
	}
	return nil
}

// settleSide works out which side this service's release is going to.
//
// Hooks run once per service and a side is a property of a target, so the two
// only line up when every routable target of the service agrees. They normally
// do — the sides move together because the service moves together — and when
// they do not, something rolled one target and not its sibling. That is a
// refusal rather than a choice: publishing a schema to one side while deploying
// to the other is the kind of wrong that looks right.
func (c *ServicePlan) settleSide(ctx context.Context, d target.Driver) []string {
	// The config decides how a service is released, not the driver. Deriving it
	// from whether a change came back carrying sides would make a driver that
	// filled them in unasked silently turn a direct service into a staged one.
	if !c.Service.Strategy.IsBlueGreen() || !c.HasWork() {
		return nil
	}

	seen := map[string]*target.Sides{}
	for _, ch := range c.Changes {
		if ch.Sides != nil {
			seen[ch.Sides.Idle.Label] = ch.Sides
		}
	}

	// A blue-green service whose routable target is already up to date, with
	// only a job to write, still has a side — the platform says so whether or
	// not this release stages anything. Reading it costs one call and means
	// {{.label}} is either always there for such a service or never, rather
	// than depending on which of its targets happened to move.
	if len(seen) == 0 {
		sides, err := c.readSide(ctx, d)
		if err != nil {
			return []string{fmt.Sprintf("services.%s: %v", c.Service.Name, err)}
		}
		seen[sides.Idle.Label] = sides
	}

	switch len(seen) {
	case 1:
		for _, sides := range seen {
			c.Side = sides.Idle.Label
			c.Previous = sides.Active.Label
		}
		return nil
	default:
		var where []string
		for _, ch := range c.Changes {
			if ch.Sides != nil {
				where = append(where, fmt.Sprintf("%s is going to %s",
					ch.Target.Label(), ch.Sides.Idle.Label))
			}
		}
		sort.Strings(where)
		return []string{fmt.Sprintf(
			"services.%s: its targets are not on the same side, so this is not one "+
				"release: %s", c.Service.Name, strings.Join(where, ", "))}
	}
}

// readSide asks the platform which side is idle, for a service that is not
// staging anything itself.
func (c *ServicePlan) readSide(ctx context.Context, d target.Driver) (*target.Sides, error) {
	r, ok := d.(target.Rollout)
	if !ok {
		return nil, fmt.Errorf("%s cannot move traffic", d.Name())
	}
	for _, t := range c.Service.Targets {
		if r.Routable(t.Type) {
			return r.Sides(ctx, t)
		}
	}
	// checkStrategy has already refused a blue-green service with nothing
	// routable, so this is unreachable rather than a case to handle.
	return nil, fmt.Errorf("no target carries traffic")
}

// checkHooks renders every hook line against the variables it will actually be
// given. See hooks.Validate for why this belongs to planning.
func (c *ServicePlan) checkHooks() []string {
	if !c.HasWork() {
		// No deploy, no hooks — so nothing to check, and a stale hook on a
		// service that is not moving is not this release's problem.
		return nil
	}

	var msgs []string
	for phase, commands := range map[string][]string{
		"before": c.Service.Before,
		"after":  c.Service.After,
	} {
		if err := hooks.Validate(commands, c.HookVars()); err != nil {
			msgs = append(msgs, fmt.Sprintf("services.%s: %s hook: %v",
				c.Service.Name, phase, err))
		}
	}
	sort.Strings(msgs)
	return msgs
}

// HookVars are the substitutions every hook of this service is given.
//
// The side variables are absent rather than empty on a direct service. That is
// the difference between a hook that fails loudly and one that publishes to
// `tst-`: tmpl.Render runs with missingkey=error, so absent is a real error and
// empty is a silent one.
func (c *ServicePlan) HookVars() hooks.Vars {
	v := hooks.Vars{}
	// The tool's own names are written last, so a --var can never shadow the
	// version actually being deployed.
	maps.Copy(v, c.Vars)
	v["version"] = c.Service.Version
	v["name"] = c.Service.Name
	v["env"] = c.Env
	if c.BlueGreen() {
		v["label"] = c.Side
		v["previous_label"] = c.Previous
	}
	return v
}

// resolveEnv builds one target's environment: the bulk envFrom maps first, then
// the explicit env on top, with references either kept for the platform or read
// here depending on what the target supports.
func resolveEnv(
	ctx context.Context,
	f *config.File,
	d target.Driver,
	c *config.Service,
	t *config.Target,
) ([]target.EnvVar, []string) {
	var problems []string
	where := fmt.Sprintf("%s/%s", t.Type, t.Name)

	resolver := d.Resolver()

	// Bulk first, so an explicit env entry always wins over the published blob.
	env := map[string]refs.Value{}
	for _, raw := range c.EnvFrom {
		ref, err := refs.ParseRef(refs.Substitute(raw, f.Env))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: envFrom: %v", where, err))
			continue
		}
		// A bulk reference carries configuration, not credentials. Allowing a
		// secret store here would make the tool read secrets on every cloud
		// instead of only where the platform leaves no choice.
		if ref.Kind != refs.Param {
			problems = append(problems, fmt.Sprintf(
				"%s: envFrom takes ${param:…}, not %s — put secrets in env as individual refs",
				where, ref.Kind))
			continue
		}
		values, err := resolver.ReadMap(ctx, ref)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: envFrom %s: %v", where, ref.Raw, err))
			continue
		}
		for k, v := range values {
			env[k] = refs.Value{Kind: refs.Literal, Literal: v, Raw: v}
		}
	}

	for name, raw := range t.Env {
		v, err := refs.Parse(refs.Substitute(raw, f.Env))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: env.%s: %v", where, name, err))
			continue
		}
		env[name] = v
	}

	out, errs := resolveValues(ctx, f, d, t, env, where, "env")
	return out, append(problems, errs...)
}

// resolveSideEnv resolves the per-side environment of a blue-green service.
//
// Same machinery as the rest of the environment, and deliberately independent of
// `ManagesEnv`: these variables are written over whatever the staged revision
// inherited rather than replacing the environment, so a service can address its
// own downstream by side while Terraform still owns everything else.
func resolveSideEnv(
	ctx context.Context,
	f *config.File,
	d target.Driver,
	t *config.Target,
) (map[string][]target.EnvVar, []string) {
	if t.Strategy == nil || len(t.Strategy.Env) == 0 {
		return nil, nil
	}

	where := fmt.Sprintf("%s/%s", t.Type, t.Name)
	out := make(map[string][]target.EnvVar, len(t.Strategy.Env))
	var problems []string
	for _, side := range sortedKeys(t.Strategy.Env) {
		env := map[string]refs.Value{}
		for name, raw := range t.Strategy.Env[side] {
			v, err := refs.Parse(refs.Substitute(raw, f.Env))
			if err != nil {
				problems = append(problems, fmt.Sprintf(
					"%s: strategy.env.%s.%s: %v", where, side, name, err))
				continue
			}
			env[name] = v
		}

		vars, errs := resolveValues(ctx, f, d, t, env, where, "strategy.env."+side)
		problems = append(problems, errs...)
		out[side] = vars
	}
	return out, problems
}

// resolveValues turns parsed values into the environment a driver is handed:
// references the platform understands are passed through (after checking they
// exist), and the rest are read here when the config allows it.
func resolveValues(
	ctx context.Context,
	f *config.File,
	d target.Driver,
	t *config.Target,
	env map[string]refs.Value,
	where, field string,
) ([]target.EnvVar, []string) {
	var problems []string
	resolver := d.Resolver()
	caps := d.Capabilities(t.Type)

	out := make([]target.EnvVar, 0, len(env))
	for _, name := range sortedKeys(env) {
		v := env[name]

		if !v.IsRef() {
			out = append(out, target.EnvVar{Name: name, Value: v})
			continue
		}

		native := (v.Kind == refs.Param && caps.NativeParam) ||
			(v.Kind == refs.Secret && caps.NativeSecret)

		if native {
			// Hand it to the platform untouched, but still confirm it exists —
			// otherwise the deploy succeeds and the container fails to start.
			slog.Debug("reference passed to the platform",
				"target", where, "name", name, "ref", v.Raw)
			if err := resolver.Verify(ctx, v); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s.%s: %v", where, field, name, err))
				continue
			}
			out = append(out, target.EnvVar{Name: name, Value: v})
			continue
		}

		if f.Refs.Resolve == config.ResolveDeny {
			problems = append(problems, fmt.Sprintf(
				"%s: %s.%s: %s cannot pass %s references to the platform, and refs.resolve is deny",
				where, field, name, t.Type, v.Kind))
			continue
		}

		// The name of what is read, never the value.
		slog.Debug("reference resolved by the tool",
			"target", where, "name", name, "ref", v.Raw)
		value, err := resolver.Read(ctx, v)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s.%s: %v", where, field, name, err))
			continue
		}
		out = append(out, target.EnvVar{
			Name:  name,
			Value: refs.Value{Kind: refs.Literal, Literal: value, Raw: v.Raw},
		})
	}

	return out, problems
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
