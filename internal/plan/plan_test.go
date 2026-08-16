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

	"github.com/evolve-platform/evolve-deploy/internal/config"
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
	// failApply names targets whose Apply should fail.
	failApply map[string]bool

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

func (d *fakeDriver) Apply(_ context.Context, ch *target.Change) error {
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
		Hooks:  &hooks.Runner{Out: io.Discard},
		Out:    io.Discard,
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

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
