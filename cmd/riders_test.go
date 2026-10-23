package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// sideDriver moves traffic and records the order things happened in, because
// the order is the point: traffic first, riders after, and nothing moved back
// when a rider fails.
type sideDriver struct {
	mu    sync.Mutex
	steps []string

	// serving is the version each label holds.
	serving map[string]string
	// riders is what each rider runs now.
	riders    map[string]string
	failApply map[string]bool
}

func newSideDriver() *sideDriver {
	return &sideDriver{
		serving:   map[string]string{"blue": "old1234", "green": "new5678"},
		riders:    map[string]string{},
		failApply: map[string]bool{},
	}
}

func (d *sideDriver) record(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.steps = append(d.steps, s)
}

func (d *sideDriver) Name() string                                     { return "fake" }
func (d *sideDriver) Capabilities(config.TargetType) target.Capability { return target.Capability{} }
func (d *sideDriver) Verify(context.Context) error                     { return nil }
func (d *sideDriver) Resolver() refs.Resolver                          { return nil }
func (d *sideDriver) Plan(context.Context, *target.Desired) (*target.Change, error) {
	return nil, nil
}
func (d *sideDriver) Revert(context.Context, *target.Change) error { return nil }

func (d *sideDriver) Apply(_ context.Context, ch *target.Change) error {
	d.record("apply:" + ch.Target.Name)
	if d.failApply[ch.Target.Name] {
		return errors.New("the revision crash looped")
	}
	d.riders[ch.Target.Name] = ch.ToVersion
	return nil
}

func (d *sideDriver) Routable(t *config.Target) bool {
	return t.Type == config.TypeContainerApp && !t.NoIngress()
}
func (d *sideDriver) Pointable(t *config.Target) bool { return d.Routable(t) }
func (d *sideDriver) Sides(context.Context, *config.Target) (*target.Sides, error) {
	return nil, nil
}
func (d *sideDriver) Stage(context.Context, *target.Change) (*target.Staged, error) {
	return nil, nil
}
func (d *sideDriver) Switch(context.Context, *target.Change) error  { return nil }
func (d *sideDriver) Abandon(context.Context, *target.Change) error { return nil }
func (d *sideDriver) Settle(context.Context, *target.Change) error  { return nil }
func (d *sideDriver) Fallback(*config.Target) string                { return "stopped" }
func (d *sideDriver) Tidy(context.Context, *config.Target) error    { return nil }

func (d *sideDriver) Point(_ context.Context, t *config.Target, label string) error {
	d.record("point:" + t.Name + ":" + label)
	return nil
}

func (d *sideDriver) Traffic(context.Context, *config.Target) ([]target.TrafficEntry, error) {
	return []target.TrafficEntry{
		entry("blue", "app--a", d.serving["blue"], 100),
		entry("green", "app--b", d.serving["green"], 0),
	}, nil
}

// restoringDriver is a sideDriver that can also bring riders back.
type restoringDriver struct{ *sideDriver }

func (d restoringDriver) RiderVersion(_ context.Context, t *config.Target) (string, error) {
	return d.riders[t.Name], nil
}

func (d restoringDriver) PlanRestore(_ context.Context, t *config.Target, version string) (*target.Change, error) {
	if d.riders[t.Name] == version {
		return nil, nil
	}
	return &target.Change{
		Service: t.Service, Target: t,
		FromVersion: d.riders[t.Name], ToVersion: version,
		Reason: "version " + d.riders[t.Name] + " -> " + version,
	}, nil
}

// purchaseWithEvents is one blue-green service with an API and a worker beside
// it, the shape a queue consumer has.
func purchaseWithEvents() (*config.File, []*config.Target) {
	strategy := &config.Strategy{Type: config.StrategyBlueGreen, Labels: config.DefaultLabels}
	api := &config.Target{Type: config.TypeContainerApp, Name: "purchase", Service: "purchase", Strategy: strategy}
	no := false
	worker := &config.Target{
		Type: config.TypeContainerApp, Name: "purchase-events", Service: "purchase",
		Strategy: strategy, Ingress: &no,
	}
	f := &config.File{
		Path: "deploy/tst.yaml",
		Services: map[string]*config.Service{
			"purchase": {Strategy: strategy, Targets: []*config.Target{api, worker}},
		},
	}
	return f, []*config.Target{api}
}

func TestTrafficMovesTheRidersAfterTheTraffic(t *testing.T) {
	d := newSideDriver()
	d.riders["purchase-events"] = "new5678"
	f, targets := purchaseWithEvents()

	var out strings.Builder
	err := moveTraffic(context.Background(), &out, restoringDriver{d}, d, f, targets, "blue")
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	want := []string{"point:purchase:blue", "apply:purchase-events"}
	if strings.Join(d.steps, ",") != strings.Join(want, ",") {
		t.Errorf("steps = %v, want %v", d.steps, want)
	}
	if d.riders["purchase-events"] != "old1234" {
		t.Errorf("the worker is on %s, want the version blue serves", d.riders["purchase-events"])
	}
}

// The traffic is the half users see, so a rider that fails does not move it
// back. The command fails and says what finishes the job.
func TestARiderThatFailsLeavesTheTrafficWhereItWent(t *testing.T) {
	d := newSideDriver()
	d.riders["purchase-events"] = "new5678"
	d.failApply["purchase-events"] = true
	f, targets := purchaseWithEvents()

	var out strings.Builder
	err := moveTraffic(context.Background(), &out, restoringDriver{d}, d, f, targets, "blue")
	if err == nil {
		t.Fatal("a rider failed and the command succeeded")
	}
	for _, w := range []string{"container-app/purchase-events", "the traffic moved",
		"evolve-deploy apply deploy/tst.yaml --only purchase --set purchase=old1234"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error does not mention %q: %v", w, err)
		}
	}
	if got := strings.Join(d.steps, ","); got != "point:purchase:blue,apply:purchase-events" {
		t.Errorf("steps = %s, want the traffic left where it went", got)
	}
}

func TestARiderAlreadyOnTheVersionIsNotWritten(t *testing.T) {
	d := newSideDriver()
	d.riders["purchase-events"] = "old1234"
	f, targets := purchaseWithEvents()

	var out strings.Builder
	if err := moveTraffic(context.Background(), &out, restoringDriver{d}, d, f, targets, "blue"); err != nil {
		t.Fatal(err)
	}
	for _, s := range d.steps {
		if strings.HasPrefix(s, "apply:") {
			t.Errorf("wrote a rider already on the version: %v", d.steps)
		}
	}
	if !strings.Contains(out.String(), "already on old1234") {
		t.Errorf("output does not say the rider was already there:\n%s", out.String())
	}
}

// A driver that cannot restore a rider still moves the traffic, and lists the
// rider with the deploy that moves it — never a silent skip, never a refusal of
// the whole rollback.
func TestARiderTheDriverCannotRestoreIsListed(t *testing.T) {
	d := newSideDriver()
	f, targets := purchaseWithEvents()

	var out strings.Builder
	if err := moveTraffic(context.Background(), &out, d, d, f, targets, "blue"); err != nil {
		t.Fatalf("the rollback was refused over a rider: %v", err)
	}
	got := out.String()
	for _, w := range []string{"container-app/purchase-events", "not moved",
		"evolve-deploy apply deploy/tst.yaml --only purchase --set purchase=old1234"} {
		if !strings.Contains(got, w) {
			t.Errorf("output does not mention %q:\n%s", w, got)
		}
	}
	if strings.Join(d.steps, ",") != "point:purchase:blue" {
		t.Errorf("steps = %v", d.steps)
	}
}

// After a rollback a rider left behind is the drift worth seeing, so it is shown
// next to the traffic it should be following.
func TestTrafficShowsTheRidersBesideTheirService(t *testing.T) {
	d := newSideDriver()
	d.riders["purchase-events"] = "new5678"
	f, targets := purchaseWithEvents()

	var out strings.Builder
	if err := showTraffic(context.Background(), &out, restoringDriver{d}, d, f, targets); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rider") || !strings.Contains(out.String(), "new5678") {
		t.Errorf("the rider and its version are not shown:\n%s", out.String())
	}
}

func TestRollbackBringsTheRidersAlong(t *testing.T) {
	d := newSideDriver()
	d.riders["purchase-events"] = "new5678"
	f, targets := purchaseWithEvents()

	steps := []rollbackStep{{
		target: targets[0],
		from:   entry("green", "app--b", "new5678", 100),
		to:     entry("blue", "app--a", "old1234", 0),
	}}
	var out strings.Builder
	if err := runRollback(context.Background(), &out, restoringDriver{d}, d, f, steps); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if d.riders["purchase-events"] != "old1234" {
		t.Errorf("the worker is on %s after a rollback", d.riders["purchase-events"])
	}
	// The plan shows the rider before anything moves.
	if !strings.Contains(out.String(), "rider") {
		t.Errorf("the rollback plan does not show the rider:\n%s", out.String())
	}
}
