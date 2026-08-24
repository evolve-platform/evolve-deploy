package plan

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// --- a driver that can also move traffic -------------------------------------

// rolloutDriver records the order of the whole choreography, because the order
// is the feature: a switch before the smoke test, or a rider written before the
// switch, is a working deploy of the wrong thing.
type rolloutDriver struct {
	*fakeDriver

	// side is what Sides reports for every target, unless idleFor overrides it.
	active, idle string
	idleFor      map[string]string

	failStage  map[string]bool
	failSwitch map[string]bool
	failSettle map[string]bool

	mu    sync.Mutex
	steps []string
}

func newRolloutDriver() *rolloutDriver {
	return &rolloutDriver{
		fakeDriver: newFakeDriver(),
		active:     "blue",
		idle:       "green",
		idleFor:    map[string]string{},
		failStage:  map[string]bool{},
		failSwitch: map[string]bool{},
		failSettle: map[string]bool{},
	}
}

// running says a target already serves this version, so a change has something
// to come from.
func (d *rolloutDriver) running(name, version string) *rolloutDriver {
	d.current[name] = version
	return d
}

func (d *rolloutDriver) record(step string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.steps = append(d.steps, step)
}

func (d *rolloutDriver) took() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.steps...)
}

func (d *rolloutDriver) Routable(t config.TargetType) bool  { return t == config.TypeECS }
func (d *rolloutDriver) Pointable(t config.TargetType) bool { return d.Routable(t) }

// fallback is what the fake reports a rollback would find, so the tests can
// pin the line without pinning one cloud's answer to it.
func (d *rolloutDriver) Fallback(t *config.Target) string {
	if t.Strategy.KeepsWarm() {
		return "warm"
	}
	return "stopped"
}

// sidesOf lets one target be a side out of step with the rest, which is the
// state a split environment is in.
func (d *rolloutDriver) sidesOf(name string) *target.Sides {
	active, idle := d.active, d.idle
	if over, ok := d.idleFor[name]; ok {
		active, idle = idle, over
	}
	return &target.Sides{
		Active: target.Side{Label: active, Revision: active + "-rev", Version: "old"},
		Idle:   target.Side{Label: idle, Revision: idle + "-rev"},
	}
}

func (d *rolloutDriver) Sides(_ context.Context, t *config.Target) (*target.Sides, error) {
	return d.sidesOf(t.Name), nil
}

// Plan mirrors the real drivers: a routable target's change carries its sides,
// a rider's does not.
func (d *rolloutDriver) Plan(ctx context.Context, want *target.Desired) (*target.Change, error) {
	ch, err := d.fakeDriver.Plan(ctx, want)
	if err != nil {
		return nil, err
	}
	staged := d.Routable(want.Target.Type) && want.Target.Strategy.IsBlueGreen()

	// Mirrors the real driver: a staged target with nothing to change is still
	// planned, as a carry, because the side it goes on has to be complete.
	if ch == nil {
		if !staged {
			return nil, nil
		}
		ch = &target.Change{
			Service:     want.Service,
			Target:      want.Target,
			FromVersion: want.Version,
			ToVersion:   want.Version,
			Carry:       true,
		}
	}
	// Sides come back only when the config asked for a staged release.
	if staged {
		ch.Sides = d.sidesOf(want.Target.Name)
	}
	ch.PublicURL = fakePublicURL(want.Target)
	return ch, nil
}

func (d *rolloutDriver) Stage(_ context.Context, ch *target.Change) (*target.Staged, error) {
	if d.failStage[ch.Target.Name] {
		d.record("stage-failed:" + ch.Target.Name)
		return nil, fmt.Errorf("the revision never became ready")
	}
	d.record("stage:" + ch.Target.Name)
	return &target.Staged{
		Label:    ch.Sides.Idle.Label,
		Revision: "new-rev",
		URL:      "https://" + ch.Target.Name + "---" + ch.Sides.Idle.Label + ".example",
		// Only Container Apps gives a revision an address of its own, so the
		// fake gives one to ecs and not to the rest — otherwise the refusal for
		// a platform that has none would never be exercised.
		RevisionURL: revisionAddress(ch.Target),
	}, nil
}

func (d *rolloutDriver) Switch(_ context.Context, ch *target.Change) error {
	if d.failSwitch[ch.Target.Name] {
		d.record("switch-failed:" + ch.Target.Name)
		return fmt.Errorf("the traffic write was rejected")
	}
	d.record("switch:" + ch.Target.Name)
	return nil
}

func (d *rolloutDriver) Abandon(_ context.Context, ch *target.Change) error {
	d.record("abandon:" + ch.Target.Name)
	return nil
}

func (d *rolloutDriver) Settle(_ context.Context, ch *target.Change) error {
	if d.failSettle[ch.Target.Name] {
		d.record("settle-failed:" + ch.Target.Name)
		return fmt.Errorf("deactivating the old revision was rejected")
	}
	d.record("settle:" + ch.Target.Name)
	return nil
}

func (d *rolloutDriver) Point(context.Context, *config.Target, string) error { return nil }

func (d *rolloutDriver) Tidy(context.Context, *config.Target) error { return nil }

func (d *rolloutDriver) Traffic(context.Context, *config.Target) ([]target.TrafficEntry, error) {
	return nil, nil
}

func (d *rolloutDriver) Apply(ctx context.Context, ch *target.Change) error {
	d.record("apply:" + ch.Target.Name)
	return d.fakeDriver.Apply(ctx, ch)
}

// runBlueGreen plans and applies one body, returning the driver and the output.
func runBlueGreen(t *testing.T, d *rolloutDriver, body string) (*rolloutDriver, string, error) {
	t.Helper()

	f := load(t, body)
	p, err := Build(context.Background(), f, d, nil)
	if err != nil {
		return d, "", err
	}

	var out strings.Builder
	err = Apply(context.Background(), p, Options{
		Driver: d,
		Hooks:  &hooks.Runner{Out: io.Discard},
		Out:    &out,
	})
	return d, out.String(), err
}

const bgService = header + `
strategy:
  type: blue-green
  smoke:
    - true

services:
  site:
    version: new
    type: ecs
    cluster: platform
`

// The order is the feature. Staging before the gate, the gate before the
// switch, and the cleanup last.
func TestChoreographyOrder(t *testing.T) {
	d, out, err := runBlueGreen(t, newRolloutDriver().running("site", "old"), bgService)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"stage:site", "switch:site", "settle:site"}
	if got := d.took(); !equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
	for _, line := range []string{
		"staged green", "smoke passed",
		"green serves new, rollback is blue old (stopped)",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("output does not mention %q:\n%s", line, out)
		}
	}
}

// The rollback line is the one thing a blue-green deploy says that a direct one
// cannot, so it has to be true: keep_warm is the difference between a rollback
// that is one write and one that starts a container first.
func TestKeepWarmChangesWhatTheRollbackIs(t *testing.T) {
	body := strings.Replace(bgService, "  type: blue-green", "  type: blue-green\n  keep_warm: true", 1)

	_, out, err := runBlueGreen(t, newRolloutDriver().running("site", "old"), body)
	if err != nil {
		t.Fatal(err)
	}

	if want := "rollback is blue old (warm)"; !strings.Contains(out, want) {
		t.Errorf("output does not mention %q:\n%s", want, out)
	}
}

// The whole point of the exercise: a gate that fails costs nothing, because
// nothing ever served from what it rejected.
func TestSmokeFailureMovesNoTraffic(t *testing.T) {
	d := newRolloutDriver()
	body := strings.Replace(bgService, "    - true", "    - exit 3", 1)

	_, out, err := runBlueGreen(t, d, body)
	if err == nil {
		t.Fatal("a failing smoke test should fail the release")
	}

	steps := d.took()
	for _, step := range steps {
		if strings.HasPrefix(step, "switch:") {
			t.Errorf("traffic was switched after a failed smoke test: %v", steps)
		}
	}
	if !contains(steps, "abandon:site") {
		t.Errorf("the staged revision was not abandoned: %v", steps)
	}
	if !strings.Contains(out, "no traffic was moved") {
		t.Errorf("output should say the traffic stayed put:\n%s", out)
	}
}

func TestStageFailureSkipsTheGate(t *testing.T) {
	d := newRolloutDriver()
	d.failStage["site"] = true

	_, _, err := runBlueGreen(t, d, bgService)
	if err == nil {
		t.Fatal("want an error")
	}

	steps := d.took()
	if !contains(steps, "abandon:site") {
		t.Errorf("nothing was abandoned: %v", steps)
	}
	for _, step := range steps {
		if strings.HasPrefix(step, "switch:") {
			t.Errorf("switched despite staging failing: %v", steps)
		}
	}
}

// A failed release is read by someone who wants two things before they want the
// stack trace: whether the old version is still serving, and whether the side
// that failed is still up and still being charged for. Both used to be missing.
// The failure was reported as "1 service(s) failed" over a list of container
// apps, and the cleanup that put everything back said nothing at all.
func TestAFailedReleaseSaysWhatIsServing(t *testing.T) {
	d := newRolloutDriver()
	d.failStage["site"] = true

	_, out, err := runBlueGreen(t, d, bgService)
	if err == nil {
		t.Fatal("want an error")
	}

	msg := err.Error()
	if !strings.Contains(msg, "the release failed while staging green") {
		t.Errorf("the error does not say what the release was doing:\n%s", msg)
	}
	if !strings.Contains(msg, "No traffic moved: blue still serves") {
		t.Errorf("the error does not say what is serving now:\n%s", msg)
	}
	// One side, one phase, one paragraph — not one row in a table of services.
	if strings.Contains(msg, "service(s) failed") {
		t.Errorf("the release was counted as a service:\n%s", msg)
	}
	if !strings.Contains(out, "discarding green, blue keeps serving") {
		t.Errorf("the cleanup was silent:\n%s", out)
	}
}

// A switch that fails is the one failure where traffic did move, so the sentence
// has to be the other one: it was put back.
func TestAFailedSwitchSaysTheTrafficWentBack(t *testing.T) {
	d := newRolloutDriver()
	d.failSwitch["site"] = true

	_, _, err := runBlueGreen(t, d, bgService)
	if err == nil {
		t.Fatal("want an error")
	}
	if msg := err.Error(); !strings.Contains(msg, "while switching to green") ||
		!strings.Contains(msg, "The traffic is back on blue") {
		t.Errorf("the error does not say where the traffic ended up:\n%s", msg)
	}
}

// Settle is cleanup. The traffic is already on the new version, so a failure
// here is a warning: removing a working version over a deactivate call that
// returned 500 is worse than the leftover.
func TestSettleFailureIsAWarning(t *testing.T) {
	d := newRolloutDriver()
	d.failSettle["site"] = true

	_, out, err := runBlueGreen(t, d, bgService)
	if err != nil {
		t.Fatalf("a failed cleanup must not fail the deploy: %v", err)
	}
	if !strings.Contains(out, "cleanup failed") {
		t.Errorf("the warning was not printed:\n%s", out)
	}
	if !strings.Contains(out, "still costs money") {
		t.Errorf("the consequence was not spelled out:\n%s", out)
	}
}

// A job shares its image with the service beside it. Writing it before the
// traffic moves points the next cron run at code the API is not serving yet.
func TestRidersMoveWithTheTraffic(t *testing.T) {
	d, _, err := runBlueGreen(t, newRolloutDriver(), header+`
strategy:
  type: blue-green

services:
  discover:
    version: new
    cluster: platform
    targets:
      - { type: ecs, name: discover }
      - { type: lambda, name: discover-sync, code: { bucket: b, key: k.zip } }
`)
	if err != nil {
		t.Fatal(err)
	}

	steps := d.took()
	stage := index(steps, "stage:discover")
	rider := index(steps, "apply:discover-sync")
	switched := index(steps, "switch:discover")

	if stage < 0 || rider < 0 || switched < 0 {
		t.Fatalf("missing a step: %v", steps)
	}
	if rider < stage {
		t.Errorf("the rider was written before the service was staged: %v", steps)
	}
}

func TestRiderFailureRollsTheTrafficBack(t *testing.T) {
	d := newRolloutDriver()
	d.failApply["discover-sync"] = true

	_, _, err := runBlueGreen(t, d, header+`
strategy:
  type: blue-green

services:
  discover:
    version: new
    cluster: platform
    targets:
      - { type: ecs, name: discover }
      - { type: lambda, name: discover-sync, code: { bucket: b, key: k.zip } }
`)
	if err == nil {
		t.Fatal("want an error")
	}

	if steps := d.took(); !contains(steps, "abandon:discover") {
		t.Errorf("the traffic was not put back after a rider failed: %v", steps)
	}
}

// A service with nothing that carries traffic cannot stage a side, and falling
// back to a direct update would ship straight to production instead of doing
// what the config asked.
func TestBlueGreenNeedsATargetThatCarriesTraffic(t *testing.T) {
	_, _, err := runBlueGreen(t, newRolloutDriver(), header+`
strategy:
  type: blue-green

services:
  jobs:
    version: new
    type: lambda
    code: { bucket: b, key: k.zip }
`)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "carries traffic") {
		t.Errorf("error = %v", err)
	}
}

func TestBlueGreenOnADriverThatCannotRouteIsRefused(t *testing.T) {
	f := load(t, bgService)
	if _, err := Build(context.Background(), f, newFakeDriver(), nil); err == nil {
		t.Fatal("want an error")
	} else if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error = %v", err)
	}
}

// --- hook variables ----------------------------------------------------------

func TestHookVarsCarryTheSide(t *testing.T) {
	d := newRolloutDriver()
	f := load(t, bgService)
	p, err := Build(context.Background(), f, d, map[string]string{"graph": "tst-blue"})
	if err != nil {
		t.Fatal(err)
	}

	vars := p.Services[0].HookVars()
	for name, want := range map[string]string{
		"label":          "green",
		"previous_label": "blue",
		"version":        "new",
		"name":           "site",
		"env":            "tst",
		"graph":          "tst-blue",
	} {
		if vars[name] != want {
			t.Errorf("{{.%s}} = %q, want %q", name, vars[name], want)
		}
	}
}

// On a direct service the side variables are absent rather than empty, so a
// hook that names one fails loudly instead of publishing to `tst-`.
func TestSideVarsAreAbsentOnADirectService(t *testing.T) {
	_, _, err := runBlueGreen(t, newRolloutDriver(), header+`
services:
  site:
    version: new
    type: ecs
    cluster: platform
    after: [ "echo {{.label}}" ]
`)
	if err == nil {
		t.Fatal("a direct service using {{.label}} should fail the plan")
	}
	if !strings.Contains(err.Error(), "after hook") {
		t.Errorf("error = %v", err)
	}
}

// A typo in an after hook must not fail a release that already succeeded.
func TestUnknownHookVariableFailsThePlan(t *testing.T) {
	d := newRolloutDriver()
	_, _, err := runBlueGreen(t, d, header+`
strategy:
  type: blue-green

services:
  site:
    version: new
    type: ecs
    cluster: platform
    after: [ "echo {{.labell}}" ]
`)
	if err == nil {
		t.Fatal("want a plan error")
	}
	if len(d.took()) != 0 {
		t.Errorf("something was deployed before the hook was checked: %v", d.took())
	}
}

// A release has no single URL, so a smoke command names what it wants. The
// names are checked while planning, because a typo there would otherwise
// surface after a staging phase that took minutes.
func TestSmokeNamesAreCheckedWhilePlanning(t *testing.T) {
	body := func(command string) string {
		return header + `
strategy:
  type: blue-green
  smoke: [ ` + fmt.Sprintf("%q", command) + ` ]

services:
  site:
    version: new
    type: ecs
    cluster: platform
`
	}

	// Only the plan is built: no url exists yet, and running it would need one.
	if _, err := Build(context.Background(),
		load(t, body(`curl -fsS {{url_stage "site"}}/healthz # {{revision "site"}}`)),
		newRolloutDriver(), nil); err != nil {
		t.Fatalf("a name this release stages should be accepted: %v", err)
	}

	_, err := Build(context.Background(),
		load(t, body(`curl {{url_stage "sight"}}`)), newRolloutDriver(), nil)
	if err == nil {
		t.Fatal("a name that stages nothing should fail the plan")
	}
	if !strings.Contains(err.Error(), "does not stage a side") ||
		!strings.Contains(err.Error(), "site") {
		t.Errorf("the error should say what is available, got %v", err)
	}

	// The release's own side is available too, and one release has exactly one.
	if _, err := Build(context.Background(),
		load(t, body("echo {{.label}} {{.previous_label}} {{.env}}")),
		newRolloutDriver(), nil); err != nil {
		t.Fatalf("the release side should be available: %v", err)
	}
}

// Writing a service name as a field is the obvious first guess, it does not
// parse, and what Go says about it is useless on its own.
func TestSmokeExplainsTheFieldMistake(t *testing.T) {
	_, err := Build(context.Background(), load(t, header+`
strategy:
  type: blue-green
  smoke: [ "curl {{.catalog-commercetools.url}}" ]

services:
  catalog-commercetools:
    version: new
    type: ecs
    cluster: platform
`), newRolloutDriver(), nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), `{{url "some-service"}}`) {
		t.Errorf("the error should say what to write instead, got %v", err)
	}
}

// One gate for the release, not one per service. Fourteen services must not run
// the same suite fourteen times.
func TestSmokeRunsOncePerRelease(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")

	d := newRolloutDriver()
	p, err := Build(context.Background(), load(t, header+`
strategy:
  type: blue-green
  smoke: [ "echo x >> `+counter+`" ]

services:
  a: { version: new, type: ecs, cluster: platform }
  b: { version: new, type: ecs, cluster: platform }
  c: { version: new, type: ecs, cluster: platform }
`), d, nil)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := Apply(context.Background(), p, Options{
		Driver: d, Hooks: &hooks.Runner{Out: io.Discard}, Out: &out,
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("the smoke command never ran: %v", err)
	}
	if n := strings.Count(string(raw), "x"); n != 1 {
		t.Errorf("the smoke commands ran %d times, want once for the release", n)
	}

	// And all three were staged before it, because the side is the environment's.
	steps := d.took()
	firstSwitch := len(steps)
	for i, step := range steps {
		if strings.HasPrefix(step, "switch:") {
			firstSwitch = i
			break
		}
	}
	for _, name := range []string{"a", "b", "c"} {
		if i := index(steps, "stage:"+name); i < 0 || i > firstSwitch {
			t.Errorf("%s was not staged before the first switch: %v", name, steps)
		}
	}
}

// url_stage resolves to the staged side, and a name that staged nothing is an
// error rather than an empty string — a gate pointed at "" would pass.
func TestSmokeUrlStageResolvesToTheStagedSide(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "url")

	d := newRolloutDriver()
	p, err := Build(context.Background(), load(t, header+`
strategy:
  type: blue-green
  smoke: [ "echo {{url_stage \"site\"}} > `+seen+`" ]

services:
  site:
    version: new
    type: ecs
    cluster: platform
`), d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), p, Options{
		Driver: d, Hooks: &hooks.Runner{Out: io.Discard}, Out: &strings.Builder{},
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "https://site---green.example" {
		t.Errorf("url = %q", got)
	}
}

// --- helpers -----------------------------------------------------------------

func revisionAddress(t *config.Target) string {
	if t.Type != config.TypeECS {
		return ""
	}
	return "https://" + t.Name + "--new-rev.example"
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func index(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

// A blue-green service whose app is already up to date but whose job is not
// still knows its side. Otherwise {{.label}} in a hook would work or fail
// depending on which of the service's targets happened to move, which is the
// worst kind of intermittent.
func TestSideIsKnownEvenWhenOnlyARiderMoves(t *testing.T) {
	d := newRolloutDriver().running("discover", "new")

	body := header + `
strategy:
  type: blue-green

services:
  discover:
    version: new
    cluster: platform
    after: [ "echo {{.label}} {{.previous_label}}" ]
    targets:
      - { type: ecs, name: discover }
      - { type: lambda, name: discover-sync, code: { bucket: b, key: k.zip } }
`

	p, err := Build(context.Background(), load(t, body), d, nil)
	if err != nil {
		t.Fatal(err)
	}

	cp := p.Services[0]
	if cp.Side != "green" || cp.Previous != "blue" {
		t.Errorf("side = %q, previous = %q", cp.Side, cp.Previous)
	}

	var out strings.Builder
	if err := Apply(context.Background(), p, Options{
		Driver: d, Hooks: &hooks.Runner{Out: io.Discard}, Out: &out,
	}); err != nil {
		t.Fatal(err)
	}

	// Nothing was staged, because nothing that carries traffic changed — but
	// the job still had to be written.
	steps := d.took()
	for _, step := range steps {
		if strings.HasPrefix(step, "stage:") || strings.HasPrefix(step, "switch:") {
			t.Errorf("nothing routable changed, yet %q happened: %v", step, steps)
		}
	}
	if !contains(steps, "apply:discover-sync") {
		t.Errorf("the job was not written: %v", steps)
	}
}

// The whole point of the model: a service that changed nothing is still staged,
// because a side missing an app is not a stack and cannot be tested as one.
func TestAnUnchangedServiceIsStagedWithTheRelease(t *testing.T) {
	d := newRolloutDriver().running("purchase", "old").running("gateway", "same")

	body := header + `
strategy:
  type: blue-green
  smoke: [ "true" ]

services:
  purchase:
    version: new
    type: ecs
    cluster: platform
  gateway:
    version: same
    type: ecs
    cluster: platform
`

	p, err := Build(context.Background(), load(t, body), d, nil)
	if err != nil {
		t.Fatal(err)
	}

	var carried bool
	for _, cp := range p.Services {
		for _, ch := range cp.Changes {
			if ch.Target.Name == "gateway" {
				carried = ch.Carry
			}
		}
	}
	if !carried {
		t.Fatal("the unchanged service was left out of the release")
	}

	var out strings.Builder
	if err := Apply(context.Background(), p, Options{
		Driver: d, Hooks: &hooks.Runner{Out: io.Discard}, Out: &out,
	}); err != nil {
		t.Fatal(err)
	}

	steps := d.took()
	if index(steps, "stage:gateway") < 0 {
		t.Errorf("the unchanged service was not staged: %v", steps)
	}
	if index(steps, "switch:gateway") < 0 {
		t.Errorf("the unchanged service did not switch with the release: %v", steps)
	}
}

// Every stage before every smoke, and every smoke before every switch. Without
// the release-wide barrier one service can already be serving while another is
// still being staged, which is exactly what the model is meant to remove.
func TestPhasesAreReleaseWide(t *testing.T) {
	d := newRolloutDriver().running("purchase", "old").running("site", "old")

	body := header + `
strategy:
  type: blue-green
  smoke: [ "true" ]

services:
  purchase:
    version: new
    type: ecs
    cluster: platform
  site:
    version: new
    type: ecs
    cluster: platform
`

	p, err := Build(context.Background(), load(t, body), d, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := Apply(context.Background(), p, Options{
		Driver: d, Hooks: &hooks.Runner{Out: io.Discard}, Out: &out,
	}); err != nil {
		t.Fatal(err)
	}

	steps := d.took()
	lastStage := max(index(steps, "stage:purchase"), index(steps, "stage:site"))
	firstSwitch := min(index(steps, "switch:purchase"), index(steps, "switch:site"))
	if lastStage < 0 || firstSwitch < 0 {
		t.Fatalf("not everything was staged and switched: %v", steps)
	}
	if firstSwitch < lastStage {
		t.Errorf("traffic moved before the whole side was staged: %v", steps)
	}
}

// A second apply must still be a no-op. Staging everything on every run would
// flip the environment and call it a deploy.
func TestNothingIsStagedWhenNothingChanged(t *testing.T) {
	d := newRolloutDriver().running("purchase", "same").running("gateway", "same")

	body := header + `
strategy:
  type: blue-green

services:
  purchase:
    version: same
    type: ecs
    cluster: platform
  gateway:
    version: same
    type: ecs
    cluster: platform
`

	p, err := Build(context.Background(), load(t, body), d, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range p.Services {
		if cp.HasWork() {
			t.Fatalf("%s has work in a release that changes nothing", cp.Service.Name)
		}
	}

	var out strings.Builder
	if err := Apply(context.Background(), p, Options{
		Driver: d, Hooks: &hooks.Runner{Out: io.Discard}, Out: &out,
	}); err != nil {
		t.Fatal(err)
	}
	if steps := d.took(); len(steps) != 0 {
		t.Errorf("something was written: %v", steps)
	}
}

// "green" has to mean the same thing everywhere, or the chain a staged side
// addresses points at whatever happens to be on the other app's green.
func TestASplitEnvironmentIsRefused(t *testing.T) {
	d := newRolloutDriver().running("purchase", "old").running("site", "old")
	d.idleFor["site"] = "blue"

	body := header + `
strategy:
  type: blue-green

services:
  purchase:
    version: new
    type: ecs
    cluster: platform
  site:
    version: new
    type: ecs
    cluster: platform
`

	_, err := Build(context.Background(), load(t, body), d, nil)
	if err == nil {
		t.Fatal("a split environment was planned")
	}
	if !strings.Contains(err.Error(), "split across both sides") ||
		!strings.Contains(err.Error(), "traffic <config> --to") {
		t.Errorf("the refusal does not say how to fix it: %v", err)
	}
}
