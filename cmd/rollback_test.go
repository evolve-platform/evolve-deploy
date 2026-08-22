package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

func entry(label, revision, version string, weight int) target.TrafficEntry {
	return target.TrafficEntry{
		Label: label, Revision: revision, Version: version, Weight: weight,
	}
}

func bgTarget(name string) *config.Target {
	return &config.Target{
		Type: config.TypeContainerApp,
		Name: name,
		Strategy: &config.Strategy{
			Type:   config.StrategyBlueGreen,
			Labels: config.DefaultLabels,
		},
	}
}

// The move is "the other side", worked out rather than typed. Everything that
// is not one clean move back is a refusal that names what it found, because
// this runs when something is already wrong.
func TestRollbackFor(t *testing.T) {
	cases := []struct {
		name    string
		entries []target.TrafficEntry
		wantTo  string
		wantErr string
	}{{
		name: "the side that is not serving",
		entries: []target.TrafficEntry{
			entry("blue", "site--rev-a", "v1", 0),
			entry("green", "site--rev-b", "v2", 100),
		},
		wantTo: "blue",
	}, {
		name: "and the other way round, since neither side is special",
		entries: []target.TrafficEntry{
			entry("blue", "site--rev-c", "v3", 100),
			entry("green", "site--rev-b", "v2", 0),
		},
		wantTo: "green",
	}, {
		name: "a split has no single side to come back from",
		entries: []target.TrafficEntry{
			entry("blue", "site--rev-a", "v1", 40),
			entry("green", "site--rev-b", "v2", 60),
		},
		wantErr: "the traffic is split",
	}, {
		name: "a first deploy has nothing behind it",
		entries: []target.TrafficEntry{
			entry("blue", "site--rev-a", "v1", 100),
		},
		wantErr: "green has never served anything",
	}, {
		// Both labels on one revision is what a settle looks like when the
		// release changed nothing. Moving traffic would be a no-op dressed up as
		// a rollback, which is worse than being told no.
		name: "both labels on the same revision",
		entries: []target.TrafficEntry{
			entry("blue", "site--rev-a", "v1", 100),
			entry("green", "site--rev-a", "v1", 0),
		},
		wantErr: "going back would change nothing",
	}, {
		name: "an unlabelled entry carrying everything",
		entries: []target.TrafficEntry{
			entry("", "site--rev-a", "v1", 100),
		},
		wantErr: "no label",
	}, {
		name: "a label the config does not know",
		entries: []target.TrafficEntry{
			entry("staging", "site--rev-a", "v1", 100),
		},
		wantErr: "not one of blue or green",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			step, err := rollbackFor(bgTarget("site"), c.entries)

			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("no error, want one mentioning %q", c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if step.to.Label != c.wantTo {
				t.Errorf("to = %q, want %q", step.to.Label, c.wantTo)
			}
		})
	}
}

// A rollback is one move for the whole environment. Half of it going back is
// worse than either version serving everywhere, so a set of moves that is not
// one move is refused rather than carried out in the order the map iterated.
func TestAgreeOnRollback(t *testing.T) {
	step := func(name, from, to, version string) rollbackStep {
		return rollbackStep{
			target: bgTarget(name),
			from:   entry(from, name+"--now", "v2", 100),
			to:     entry(to, name+"--was", version, 0),
		}
	}

	if err := agreeOnRollback([]rollbackStep{
		step("site", "green", "blue", "v1"),
		step("api", "green", "blue", "v1"),
	}); err != nil {
		t.Errorf("two targets moving the same way is one rollback: %v", err)
	}

	err := agreeOnRollback([]rollbackStep{
		step("site", "green", "blue", "v1"),
		step("api", "blue", "green", "v1"),
	})
	if err == nil || !strings.Contains(err.Error(), "do not agree on which side") {
		t.Errorf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "site") || !strings.Contains(err.Error(), "api") {
		t.Errorf("the refusal should show what it found:\n%v", err)
	}

	err = agreeOnRollback([]rollbackStep{
		step("site", "green", "blue", "v1"),
		step("api", "green", "blue", "v0"),
	})
	if err == nil || !strings.Contains(err.Error(), "not hold the same version") {
		t.Errorf("error = %v", err)
	}
}

// The two shapes of rollback are picked by what the driver says about itself,
// not by naming a cloud in the command.
func TestUndoableTargetsPicksWhatCannotBePointedAt(t *testing.T) {
	f := &config.File{
		Services: map[string]*config.Service{
			"site": {
				Name:     "site",
				Strategy: &config.Strategy{Type: config.StrategyBlueGreen},
				Targets: []*config.Target{
					{Type: config.TypeECS, Name: "site", Service: "site"},
					// A rider: no listener rule, nothing to reverse on its own.
					{Type: config.TypeLambda, Name: "site-events", Service: "site"},
				},
			},
			// A direct service is not part of any of this.
			"legacy": {
				Name:     "legacy",
				Strategy: &config.Strategy{Type: config.StrategyDirect},
				Targets:  []*config.Target{{Type: config.TypeECS, Name: "legacy"}},
			},
		},
	}

	got := undoableTargets(f, delegatingRollout{}, delegatingDriver{})
	if len(got) != 1 || got[0].Name != "site" {
		t.Fatalf("undoable = %v, want just the ECS target of the blue-green service", got)
	}

	// A driver with a standing side to point at has nothing to undo: rollback
	// takes the other path there, and offering both would be two rollbacks.
	if got := undoableTargets(f, pointingRollout{}, delegatingDriver{}); len(got) != 0 {
		t.Errorf("undoable = %v, want none when the sides can be pointed at", got)
	}

	// And a driver that cannot reverse anything is not asked to.
	if got := undoableTargets(f, delegatingRollout{}, plainDriver{}); len(got) != 0 {
		t.Errorf("undoable = %v, want none when the driver is no Undoer", got)
	}
}

// The fakes below implement only what undoableTargets reaches for.

type delegatingRollout struct{ target.Rollout }

func (delegatingRollout) Routable(t config.TargetType) bool { return t == config.TypeECS }
func (delegatingRollout) Pointable(config.TargetType) bool  { return false }

type pointingRollout struct{ target.Rollout }

func (pointingRollout) Routable(t config.TargetType) bool  { return t == config.TypeECS }
func (pointingRollout) Pointable(t config.TargetType) bool { return t == config.TypeECS }

type delegatingDriver struct{ target.Driver }

func (delegatingDriver) Undo(context.Context, *config.Target) (string, error) {
	return "", target.ErrNoWindow
}

type plainDriver struct{ target.Driver }
