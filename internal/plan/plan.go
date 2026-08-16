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
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/evolve-platform/evolve-deploy/internal/config"
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
}

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
func Build(ctx context.Context, f *config.File, d target.Driver) (*Plan, error) {
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
			cp, errs := planService(gctx, f, d, f.Services[name])
			if len(errs) > 0 {
				report(errs...)
				return nil
			}
			plans[i] = cp
			return nil
		})
	}
	_ = g.Wait()

	p := &Plan{File: f}
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
	return p, nil
}

// planService resolves one service's targets and asks the driver what would
// change. Its targets are planned concurrently too, which matters for something
// like catalog-commercetools: one service and four jobs sharing an image.
func planService(
	ctx context.Context,
	f *config.File,
	d target.Driver,
	c *config.Service,
) (*ServicePlan, []string) {
	var problems []string

	cp := &ServicePlan{Service: c, Env: f.Env}
	desired := make([]*target.Desired, 0, len(c.Targets))

	for _, t := range c.Targets {
		slog.Debug("planning", "service", c.Name,
			"target", fmt.Sprintf("%s/%s", t.Type, t.Name), "version", c.Version)

		env, errs := resolveEnv(ctx, f, d, c, t)
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
		cp.Changes = append(cp.Changes, ch)
	}
	return cp, nil
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
	caps := d.Capabilities(t.Type)

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
				problems = append(problems, fmt.Sprintf("%s: env.%s: %v", where, name, err))
				continue
			}
			out = append(out, target.EnvVar{Name: name, Value: v})
			continue
		}

		if f.Refs.Resolve == config.ResolveDeny {
			problems = append(problems, fmt.Sprintf(
				"%s: env.%s: %s cannot pass %s references to the platform, and refs.resolve is deny",
				where, name, t.Type, v.Kind))
			continue
		}

		// The name of what is read, never the value.
		slog.Debug("reference resolved by the tool",
			"target", where, "name", name, "ref", v.Raw)
		value, err := resolver.Read(ctx, v)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: env.%s: %v", where, name, err))
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
