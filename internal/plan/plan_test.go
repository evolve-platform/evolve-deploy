package plan

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/console"
	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// --- a driver that records what it was asked to do ---------------------------

type fakeDriver struct {
	caps map[config.TargetType]target.Capability
	res  *fakeResolver

	// current maps target name to the version it runs; absent means "not
	// deployed yet".
	current map[string]string

	// available maps target name to the versions that have an artifact, for
	// Artifacts.
	available map[string][]string
	// failApply names targets whose Apply should fail.
	failApply map[string]bool
	// waitFor makes Apply block, and waitingOn what it then claims to be
	// waiting for — the two things a progress line is made of.
	waitFor   time.Duration
	waitingOn string

	mu       sync.Mutex
	applied  []string
	reverted []string
	env      map[string][]target.EnvVar
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{
		caps:      map[config.TargetType]target.Capability{},
		res:       &fakeResolver{params: map[string]string{}, secrets: map[string]string{}},
		current:   map[string]string{},
		failApply: map[string]bool{},
		env:       map[string][]target.EnvVar{},
	}
}

func (d *fakeDriver) Name() string                 { return "fake" }
func (d *fakeDriver) Verify(context.Context) error { return nil }
func (d *fakeDriver) Resolver() refs.Resolver      { return d.res }
func (d *fakeDriver) Capabilities(t config.TargetType) target.Capability {
	return d.caps[t]
}

func (d *fakeDriver) Plan(_ context.Context, want *target.Desired) (*target.Change, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.env[want.Target.Name] = want.Env

	from := d.current[want.Target.Name]
	if from == want.Version {
		return nil, nil
	}
	return &target.Change{
		Service:     want.Service,
		Target:      want.Target,
		FromVersion: from,
		ToVersion:   want.Version,
		Reason:      "version changed",
	}, nil
}

// Artifacts is only asked by `update`, never by a plan, so the fake answers
// with whatever it was given and the interface stays satisfied.
func (d *fakeDriver) Artifacts(
	_ context.Context, t *config.Target, versions []string,
) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	have := map[string]bool{}
	for _, v := range d.available[t.Name] {
		have[v] = true
	}
	return target.Present(versions, have), nil
}

func (d *fakeDriver) Apply(ctx context.Context, ch *target.Change) error {
	if d.waitFor > 0 {
		if d.waitingOn != "" {
			target.Status(ctx, "%s", d.waitingOn)
		}
		select {
		case <-time.After(d.waitFor):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failApply[ch.Target.Name] {
		return fmt.Errorf("boom")
	}
	d.applied = append(d.applied, ch.Target.Name)
	return nil
}

func (d *fakeDriver) Revert(_ context.Context, ch *target.Change) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reverted = append(d.reverted, ch.Target.Name)
	return nil
}

type fakeResolver struct {
	params  map[string]string
	secrets map[string]string
	// reads records every value the tool actually read, which is how the tests
	// check that nothing was read that should have been passed through.
	reads []string
	mu    sync.Mutex
}

func (r *fakeResolver) Verify(_ context.Context, v refs.Value) error {
	if _, ok := r.lookup(v); !ok {
		return fmt.Errorf("%s does not exist", v.Raw)
	}
	return nil
}

func (r *fakeResolver) Read(_ context.Context, v refs.Value) (string, error) {
	val, ok := r.lookup(v)
	if !ok {
		return "", fmt.Errorf("%s does not exist", v.Raw)
	}
	r.mu.Lock()
	r.reads = append(r.reads, v.Raw)
	r.mu.Unlock()
	return val, nil
}

func (r *fakeResolver) ReadMap(ctx context.Context, v refs.Value) (map[string]string, error) {
	raw, err := r.Read(ctx, v)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		k, val, _ := strings.Cut(pair, "=")
		out[k] = val
	}
	return out, nil
}

func (r *fakeResolver) lookup(v refs.Value) (string, bool) {
	if v.Kind == refs.Secret {
		val, ok := r.secrets[v.Name]
		return val, ok
	}
	val, ok := r.params[v.Name]
	return val, ok
}

// --- helpers -----------------------------------------------------------------

func load(t *testing.T, body string) *config.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tst.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, "tst")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

const header = `
cloud:
  provider: aws
  account: "1234"
  region: eu-west-1
`

func envOf(t *testing.T, d *fakeDriver, name string) map[string]refs.Value {
	t.Helper()
	out := map[string]refs.Value{}
	for _, e := range d.env[name] {
		out[e.Name] = e.Value
	}
	return out
}

// --- tests -------------------------------------------------------------------

func TestReferencesArePassedThroughWhenTheTargetSupportsThem(t *testing.T) {
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}
	d.res.secrets["purchase/ctp"] = "super-secret"
	d.res.params["/platform/redis"] = "redis://host"

	f := load(t, header+`
services:
  purchase:
    version: v2
    type: ecs
    cluster: platform
    env:
      LOG_LEVEL: info
      REDIS_URL: ${param:/platform/redis}
      CTP_CLIENT_SECRET: ${secret:purchase/ctp}
`)

	if _, err := Build(context.Background(), f, d); err != nil {
		t.Fatal(err)
	}

	env := envOf(t, d, "purchase")
	if !env["REDIS_URL"].IsRef() || !env["CTP_CLIENT_SECRET"].IsRef() {
		t.Error("references were resolved even though the target can carry them")
	}
	if env["LOG_LEVEL"].Literal != "info" {
		t.Errorf("LOG_LEVEL = %+v", env["LOG_LEVEL"])
	}
	// The point of the capability table: on a target that resolves references
	// itself, no secret value ever passes through this process.
	if len(d.res.reads) != 0 {
		t.Errorf("the tool read %v, but ECS resolves references itself", d.res.reads)
	}
}

func TestReferencesAreResolvedWhenTheTargetCannotCarryThem(t *testing.T) {
	d := newFakeDriver()
	d.caps[config.TypeLambda] = target.Capability{} // Lambda carries nothing
	d.res.secrets["purchase/ctp"] = "super-secret"

	f := load(t, header+`
services:
  purchase:
    version: v2
    type: lambda
    code: { bucket: b, key: "k-{{.version}}.zip" }
    env:
      CTP_CLIENT_SECRET: ${secret:purchase/ctp}
`)

	if _, err := Build(context.Background(), f, d); err != nil {
		t.Fatal(err)
	}

	env := envOf(t, d, "purchase")
	if env["CTP_CLIENT_SECRET"].IsRef() {
		t.Fatal("a reference was handed to a lambda, which cannot resolve one")
	}
	if env["CTP_CLIENT_SECRET"].Literal != "super-secret" {
		t.Errorf("value = %q", env["CTP_CLIENT_SECRET"].Literal)
	}
	// Raw is kept so the diff can still show where the value came from without
	// printing it.
	if env["CTP_CLIENT_SECRET"].Raw != "${secret:purchase/ctp}" {
		t.Errorf("Raw = %q, want the original reference", env["CTP_CLIENT_SECRET"].Raw)
	}
}

func TestResolveDenyRefusesRatherThanReading(t *testing.T) {
	d := newFakeDriver()
	d.caps[config.TypeLambda] = target.Capability{}
	d.res.secrets["purchase/ctp"] = "super-secret"

	f := load(t, header+`
refs:
  resolve: deny
services:
  purchase:
    version: v2
    type: lambda
    code: { bucket: b, key: "k.zip" }
    env:
      CTP_CLIENT_SECRET: ${secret:purchase/ctp}
`)

	_, err := Build(context.Background(), f, d)
	if err == nil {
		t.Fatal("expected the plan to fail under refs.resolve: deny")
	}
	if !strings.Contains(err.Error(), "refs.resolve is deny") {
		t.Errorf("error was: %v", err)
	}
	if len(d.res.reads) != 0 {
		t.Errorf("a value was read despite the deny policy: %v", d.res.reads)
	}
}

func TestEnvFromIsOverriddenByExplicitEnv(t *testing.T) {
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}
	d.res.params["/evolve/tst/purchase/setup"] = "LOG_LEVEL=warn,REDIS_URL=redis://from-terraform"

	f := load(t, header+`
services:
  purchase:
    version: v2
    type: ecs
    cluster: platform
    envFrom:
      - ${param:/evolve/${env}/purchase/setup}
    env:
      LOG_LEVEL: debug
`)

	if _, err := Build(context.Background(), f, d); err != nil {
		t.Fatal(err)
	}

	env := envOf(t, d, "purchase")
	if env["LOG_LEVEL"].Literal != "debug" {
		t.Errorf("LOG_LEVEL = %q, want the explicit env to win", env["LOG_LEVEL"].Literal)
	}
	if env["REDIS_URL"].Literal != "redis://from-terraform" {
		t.Errorf("REDIS_URL = %q, want the value from the bulk blob", env["REDIS_URL"].Literal)
	}
}

func TestEnvFromRejectsASecretStore(t *testing.T) {
	// A bulk reference carries configuration. Allowing a secret store would
	// make the tool read secrets on every cloud, not only where the platform
	// leaves no choice.
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}
	d.res.secrets["bundle"] = "A=1"

	f := load(t, header+`
services:
  purchase:
    version: v2
    type: ecs
    cluster: platform
    envFrom:
      - ${secret:bundle}
`)

	_, err := Build(context.Background(), f, d)
	if err == nil || !strings.Contains(err.Error(), "envFrom takes ${param:") {
		t.Fatalf("error was: %v", err)
	}
}

func TestPlanFailsBeforeAnythingIsDeployed(t *testing.T) {
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}

	f := load(t, header+`
services:
  good:
    version: v2
    type: ecs
    cluster: platform
  broken:
    version: v2
    type: ecs
    cluster: platform
    env:
      MISSING: ${param:/nope}
`)

	_, err := Build(context.Background(), f, d)
	if err == nil {
		t.Fatal("expected the plan to fail")
	}
	if !strings.Contains(err.Error(), "nothing was deployed") {
		t.Errorf("error should say nothing happened, got: %v", err)
	}
	if len(d.applied) != 0 {
		t.Errorf("applied %v despite a broken plan", d.applied)
	}
}

func TestUnchangedTargetsAreSkipped(t *testing.T) {
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}
	d.current["site"] = "v2"

	f := load(t, header+`
services:
  site:
    version: v2
    type: ecs
    cluster: platform
`)

	p, err := Build(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Error("a target already at its version produced work")
	}
	if len(p.Services[0].Unchanged) != 1 {
		t.Errorf("Unchanged = %v", p.Services[0].Unchanged)
	}
}

func TestAFailedTargetRevertsItsSiblings(t *testing.T) {
	// The service is the rollback boundary: its targets run the same image
	// and may have a contract with each other, so they move together.
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}
	d.caps[config.TypeLambda] = target.Capability{}
	d.failApply["purchase-events"] = true

	f := load(t, header+`
services:
  purchase:
    version: v2
    targets:
      - { type: ecs, name: purchase, cluster: platform }
      - type: lambda
        name: purchase-events
        code: { bucket: b, key: "k.zip" }
  site:
    version: v2
    type: ecs
    cluster: platform
`)

	p, err := Build(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}

	err = Apply(context.Background(), p, Options{
		Driver: d,
		Hooks:  &hooks.Runner{Log: discardLog()},
		Log:    discardLog(),
	})
	if err == nil {
		t.Fatal("expected the apply to fail")
	}

	if !contains(d.reverted, "purchase") {
		t.Errorf("reverted = %v, want the succeeded sibling to be put back", d.reverted)
	}
	// A service that succeeded is left alone: undoing a healthy deploy
	// because of a failure elsewhere is more disruption than it fixes.
	if contains(d.reverted, "site") {
		t.Error("an unrelated service was rolled back")
	}
	if !contains(d.applied, "site") {
		t.Errorf("applied = %v, want site to have been deployed", d.applied)
	}
}

func TestASlowTargetSaysItIsStillWaiting(t *testing.T) {
	// Two minutes of silence while a container passes its probes is
	// indistinguishable from a hang, and someone cancels a release that was
	// going fine.
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}
	d.waitFor = 120 * time.Millisecond
	d.waitingOn = "health"

	p, err := Build(context.Background(), load(t, header+`
services:
  purchase:
    version: v2
    type: ecs
    cluster: platform
`), d)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	log := console.New(&out, 0, 0)
	if err := Apply(context.Background(), p, Options{
		Driver:     d,
		Hooks:      &hooks.Runner{Log: log},
		Log:        log,
		WaitNotice: 20 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "still waiting on health") {
		t.Errorf("no progress line, and the driver said what it was waiting for:\n%s", out.String())
	}
	// And it stops when the target does: a line after the result would read as
	// a target that is somehow both finished and still going.
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	for i, line := range lines {
		if strings.Contains(line, "v2 in ") {
			for _, after := range lines[i+1:] {
				if strings.Contains(after, "still waiting") {
					t.Errorf("progress line after the target finished:\n%s", out.String())
				}
			}
		}
	}
}

func TestAQuickTargetSaysNothingExtra(t *testing.T) {
	// The line only exists to break a silence. A target that returns straight
	// away has not been silent.
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}

	p, err := Build(context.Background(), load(t, header+`
services:
  purchase:
    version: v2
    type: ecs
    cluster: platform
`), d)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	log := console.New(&out, 0, 0)
	if err := Apply(context.Background(), p, Options{
		Driver:     d,
		Hooks:      &hooks.Runner{Log: log},
		Log:        log,
		WaitNotice: time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "still waiting") {
		t.Errorf("a target that finished at once printed a progress line:\n%s", out.String())
	}
}

// discardLog is the output nearly every test wants: it has to exist, and what
// it prints is checked elsewhere.
func discardLog() *console.Log { return console.New(io.Discard, 0, 0) }

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func TestABrokenBeforeHookStopsTheWholeRelease(t *testing.T) {
	// The gate is the release, not the service. A schema check that fails on
	// purchase means the release is already lost, so site must not go out
	// either — half a version deployed is the outcome nobody asked for.
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}

	f := load(t, header+`
services:
  purchase:
    version: v2
    type: ecs
    cluster: platform
    before: ["exit 1"]
  site:
    version: v2
    type: ecs
    cluster: platform
    before: ["true"]
`)

	p, err := Build(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}

	err = Apply(context.Background(), p, Options{
		Driver: d,
		Hooks:  &hooks.Runner{Log: discardLog()},
		Log:    discardLog(),
	})
	if err == nil {
		t.Fatal("expected the apply to fail")
	}
	if !strings.Contains(err.Error(), "nothing was deployed") {
		t.Errorf("the error should say the release was a no-op, got: %v", err)
	}
	if len(d.applied) != 0 {
		t.Errorf("applied %v after a before hook failed", d.applied)
	}
	if len(d.reverted) != 0 {
		t.Errorf("reverted %v, but nothing was ever deployed", d.reverted)
	}
}

func TestEveryBeforeHookRunsBeforeTheRunIsCalledOff(t *testing.T) {
	// Someone who broke three schemas should see three messages, not fix one
	// and discover the next on the following run.
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}

	f := load(t, header+`
services:
  purchase:
    version: v2
    type: ecs
    cluster: platform
    before: ["exit 1"]
  site:
    version: v2
    type: ecs
    cluster: platform
    before: ["exit 1"]
`)

	p, err := Build(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}

	err = Apply(context.Background(), p, Options{
		Driver: d,
		Hooks:  &hooks.Runner{Log: discardLog()},
		Log:    discardLog(),
	})
	if err == nil {
		t.Fatal("expected the apply to fail")
	}
	for _, name := range []string{"purchase", "site"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s is missing from the error: %v", name, err)
		}
	}
}

func TestAfterHooksFollowTheirOwnService(t *testing.T) {
	// after is per service and only on success: a service that failed has
	// nothing to publish, and one that worked should not be held back by it.
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}
	d.failApply["purchase"] = true

	f := load(t, header+`
services:
  purchase:
    version: v2
    type: ecs
    cluster: platform
    after: ["echo published-purchase"]
  site:
    version: v2
    type: ecs
    cluster: platform
    after: ["echo published-site"]
`)

	p, err := Build(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err = Apply(context.Background(), p, Options{
		Driver: d,
		Hooks:  &hooks.Runner{Log: console.New(&out, 0, 0)},
		Log:    console.New(io.Discard, 0, 0),
	})
	if err == nil {
		t.Fatal("expected the apply to fail")
	}
	if strings.Contains(out.String(), "published-purchase") {
		t.Error("the after hook of a failed service ran")
	}
	if !strings.Contains(out.String(), "published-site") {
		t.Error("the after hook of a service that succeeded did not run")
	}
}

func TestADependencyDeploysFirst(t *testing.T) {
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}

	f := load(t, header+`
services:
  site:
    version: v2
    type: ecs
    cluster: platform
    depends_on: [discover, purchase]
  discover:
    version: v2
    type: ecs
    cluster: platform
  purchase:
    version: v2
    type: ecs
    cluster: platform
`)

	p, err := Build(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), p, Options{
		Driver: d,
		Hooks:  &hooks.Runner{Log: discardLog()},
		Log:    discardLog(),
	}); err != nil {
		t.Fatal(err)
	}

	// The two backends may land in either order, but the frontend has to be
	// last — that is the whole point of naming them.
	if got := slices.Index(d.applied, "site"); got != len(d.applied)-1 {
		t.Errorf("applied = %v, want site last", d.applied)
	}
}

func TestAServiceIsSkippedWhenWhatItNeedsFails(t *testing.T) {
	// Deploying a frontend against a backend that just failed is exactly what
	// the ordering is there to prevent, so the frontend must not go out at all.
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}
	d.failApply["discover"] = true

	f := load(t, header+`
services:
  site:
    version: v2
    type: ecs
    cluster: platform
    depends_on: [discover]
  discover:
    version: v2
    type: ecs
    cluster: platform
  unrelated:
    version: v2
    type: ecs
    cluster: platform
`)

	p, err := Build(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}
	err = Apply(context.Background(), p, Options{
		Driver: d,
		Hooks:  &hooks.Runner{Log: discardLog()},
		Log:    discardLog(),
	})
	if err == nil {
		t.Fatal("expected the apply to fail")
	}

	if contains(d.applied, "site") {
		t.Errorf("applied = %v, want site left alone", d.applied)
	}
	// It never ran, so it is not a failure and must not be counted as one.
	if !strings.Contains(err.Error(), "1 service(s) failed") {
		t.Errorf("skipped services were counted as failures: %v", err)
	}
	if !strings.Contains(err.Error(), "site, waiting on discover") {
		t.Errorf("the error does not say why site never ran: %v", err)
	}
	// A service that depends on nothing is unaffected by someone else's bad day.
	if !contains(d.applied, "unrelated") {
		t.Errorf("applied = %v, want unrelated to have gone out", d.applied)
	}
}

func TestADependencyOutsideTheRunIsAlreadySatisfied(t *testing.T) {
	// The CI case: the pipeline deploys only what it rebuilt, so the backend a
	// frontend names is usually not in the run at all. That must not block it.
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}
	d.current["discover"] = "v2"

	f := load(t, header+`
services:
  site:
    version: v2
    type: ecs
    cluster: platform
    depends_on: [discover]
  discover:
    version: v2
    type: ecs
    cluster: platform
`)

	p, err := Build(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), p, Options{
		Driver: d,
		Hooks:  &hooks.Runner{Log: discardLog()},
		Log:    discardLog(),
	}); err != nil {
		t.Fatal(err)
	}
	if !contains(d.applied, "site") {
		t.Errorf("applied = %v, want site deployed despite discover not being in the run", d.applied)
	}
}

func TestALongerChainDoesNotDeadlockOnWorkers(t *testing.T) {
	// A chain longer than the worker limit is the case that deadlocks if a
	// service claims its slot before waiting on what it depends on.
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeParam: true, NativeSecret: true}

	body := header + "services:\n"
	for i := range 5 {
		body += fmt.Sprintf("  svc%d:\n    version: v2\n    type: ecs\n    cluster: platform\n", i)
		if i > 0 {
			body += fmt.Sprintf("    depends_on: [svc%d]\n", i-1)
		}
	}

	p, err := Build(context.Background(), load(t, body), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), p, Options{
		Driver:      d,
		Hooks:       &hooks.Runner{Log: discardLog()},
		Log:         discardLog(),
		Concurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"svc0", "svc1", "svc2", "svc3", "svc4"}; !slices.Equal(d.applied, want) {
		t.Errorf("applied = %v, want %v", d.applied, want)
	}
}
