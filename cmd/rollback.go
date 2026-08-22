package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback <config>",
	Short: "Move all the traffic back to the side that was serving before",
	Long: `Rollback undoes the last release.

It has two shapes, because the platforms do, and it picks the right one from the
config rather than from a flag.

Where the tool owns the sides — Container Apps, Cloud Run — this is traffic --to
without having to know the label. A release moves every blue-green service to
the same side at once, so going back is the same move in reverse: whichever side
is not serving is the previous version, and this hands it everything.

It works out which side that is per target and refuses if they disagree, because
half an environment on the old version is worse than either version. It refuses
just as loudly when the traffic is already split, when a side has nothing to go
back to, and when the two sides do not hold the same version everywhere.

The previous version is normally stopped after a switch — it keeps its label,
not its replicas — so this starts it and waits for it before moving anything.
That is a container start, not the instant flip you get with keep_warm, and it
is why this prints what it is doing rather than returning silently.

On a platform that owns its own sides there is nothing to name, so this reverses
the release instead: on ECS, for as long as the deployment has not finished, the
previous version is still running and ECS can put the traffic back on it. That
covers the bake_time window after a switch, and a release whose pipeline died
while paused at the smoke gate. Once the deployment has finished the old tasks
are gone and going back is a deploy — which this says, with the command in it,
rather than failing.

--only narrows it to some of the services, which is how you take one back
without taking the environment back. It is deliberately something you have to
ask for: the refusal above is there because half an environment on the old
version is usually a mistake, and this is how you say it is not.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		out := cmd.OutOrStdout()

		f, err := loadConfig(args[0])
		if err != nil {
			return err
		}
		driver, err := newDriver(ctx, f)
		if err != nil {
			return err
		}
		rollout, ok := driver.(target.Rollout)
		if !ok {
			return fmt.Errorf("%s cannot move traffic by label", driver.Name())
		}
		if err := driver.Verify(ctx); err != nil {
			return err
		}

		// Two shapes, because the platforms genuinely differ. A side that
		// stands still is pointed at; a platform that owns its own sides is
		// asked to reverse what it is still running. A config is one cloud, so
		// in practice exactly one of these lists has anything in it.
		targets := pointableTargets(f, rollout)
		undoable := undoableTargets(f, rollout, driver)
		if len(targets)+len(undoable) == 0 {
			return noSidesToMove(f, driver.Name())
		}

		var problems []string
		if len(targets) > 0 {
			steps, err := planRollback(ctx, rollout, targets)
			if err != nil {
				return err
			}
			if err := runRollback(ctx, out, rollout, steps); err != nil {
				problems = append(problems, err.Error())
			}
		}
		if len(undoable) > 0 {
			if err := runUndo(ctx, out, driver.(target.Undoer), undoable, f.Path); err != nil {
				problems = append(problems, err.Error())
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("%s", strings.Join(problems, "\n"))
		}
		return nil
	},
}

// undoableTargets is every target whose release can be reversed by asking the
// platform, rather than by moving traffic to a side.
func undoableTargets(
	f *config.File,
	r target.Rollout,
	driver target.Driver,
) []*config.Target {
	if _, ok := driver.(target.Undoer); !ok {
		return nil
	}

	var out []*config.Target
	for _, name := range f.ServiceNames() {
		c := f.Services[name]
		if c == nil || !c.Strategy.IsBlueGreen() {
			continue
		}
		for _, t := range c.Targets {
			// Carries traffic, but has no side that stands still to be named.
			if r.Routable(t.Type) && !r.Pointable(t.Type) {
				out = append(out, t)
			}
		}
	}
	return out
}

// runUndo asks the platform to reverse what it is still running.
//
// A target with nothing left to reverse is not a failure. It is the normal
// state of anything deployed more than a bake time ago, and the only useful
// thing to say about it is what to do instead — so it prints that and does not
// colour the run red.
func runUndo(
	ctx context.Context,
	out io.Writer,
	u target.Undoer,
	targets []*config.Target,
	path string,
) error {
	fmt.Fprintf(out, "Reversing the release on %d target(s):\n", len(targets))
	for _, t := range targets {
		fmt.Fprintf(out, "  %s\n", t.Label())
	}
	fmt.Fprintln(out)

	var (
		problems []string
		closed   []*config.Target
	)
	for _, t := range targets {
		// Printed before rather than after: this waits for the platform to
		// shift traffic back, and a line that appears only once it returns is a
		// line that appears after the silence it was meant to explain.
		fmt.Fprintf(out, "  %-40s asking the platform to roll back\n", t.Label())

		what, err := u.Undo(ctx, t)
		switch {
		case errors.Is(err, target.ErrNoWindow):
			closed = append(closed, t)
			fmt.Fprintf(out, "  %-40s nothing left to reverse\n", t.Label())
		case err != nil:
			fmt.Fprintf(out, "  %-40s failed: %v\n", t.Label(), err)
			problems = append(problems, fmt.Sprintf("%s: %v", t.Label(), err))
		default:
			fmt.Fprintf(out, "  %-40s %s\n", t.Label(), what)
		}
	}

	if len(closed) > 0 {
		fmt.Fprintf(out, "\n%s\n", windowClosed(closed, path))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%d target(s) were not rolled back:\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// windowClosed explains the state this cannot fix, and names the one thing that
// can. Whoever runs a rollback during an outage should not have to go and read
// how the platform they are on differs.
func windowClosed(targets []*config.Target, path string) string {
	services := map[string]bool{}
	for _, t := range targets {
		services[t.Service] = true
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	return fmt.Sprintf(
		"Those releases have finished, so the platform has already terminated the\n"+
			"version they replaced — there is nothing standing to put the traffic back on.\n"+
			"Going back is a deploy of the previous version:\n"+
			"  evolve-deploy apply %s --only %s --set %s=<previous version>",
		path, strings.Join(names, ","), names[0])
}

// rollbackStep is one target and the move it is about to make.
type rollbackStep struct {
	target *config.Target
	from   target.TrafficEntry
	to     target.TrafficEntry
}

// planRollback works out the move for every target and refuses anything it
// cannot state plainly.
//
// It reads the traffic block rather than going through Sides for the same
// reason traffic does: this runs when something is already wrong, and a check
// that refuses to interpret the state is no use to whoever is trying to undo
// it. What it will not do is guess — every refusal below names the state it
// found and the command that resolves it.
func planRollback(
	ctx context.Context,
	r target.Rollout,
	targets []*config.Target,
) ([]rollbackStep, error) {
	var (
		steps    []rollbackStep
		problems []string
	)

	for _, t := range targets {
		entries, err := r.Traffic(ctx, t)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", t.Label(), err))
			continue
		}

		step, err := rollbackFor(t, entries)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", t.Label(), err))
			continue
		}
		steps = append(steps, *step)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("nothing was moved:\n  - %s", strings.Join(problems, "\n  - "))
	}
	if err := agreeOnRollback(steps); err != nil {
		return nil, err
	}
	return steps, nil
}

// agreeOnRollback refuses a set of moves that is not one move.
//
// A release puts every blue-green service on the same side at the same moment,
// so undoing it is one move in reverse. Two targets that would go back to
// different sides, or to different versions, mean a release died between them —
// and finishing that job in the other direction is not what anyone typed this
// for.
func agreeOnRollback(steps []rollbackStep) error {
	sides := map[string]bool{}
	versions := map[string]bool{}
	for _, s := range steps {
		sides[s.to.Label] = true
		versions[s.to.Version] = true
	}

	if len(sides) > 1 {
		names := make([]string, 0, len(sides))
		for label := range sides {
			names = append(names, label)
		}
		sort.Strings(names)
		return fmt.Errorf(
			"the targets do not agree on which side to go back to (%s):\n%s\n"+
				"    put them on one side first with `evolve-deploy traffic <config> --to <label>`",
			strings.Join(names, " and "), describeSteps(steps))
	}

	// Going back to two different versions is not one rollback, it is two. The
	// versions are the whole point of the line printed before the move, so a
	// mismatch has to stop here rather than be printed and carried out.
	if len(versions) > 1 {
		return fmt.Errorf(
			"the idle side does not hold the same version everywhere:\n%s\n"+
				"    a rollback moves the environment as one, so resolve this per target with\n"+
				"    `evolve-deploy traffic <config> --to <label>`", describeSteps(steps))
	}
	return nil
}

// rollbackFor picks the move for one target's traffic block.
func rollbackFor(t *config.Target, entries []target.TrafficEntry) (*rollbackStep, error) {
	byLabel := map[string]target.TrafficEntry{}
	var serving []target.TrafficEntry
	for _, e := range entries {
		if e.Weight > 0 {
			serving = append(serving, e)
		}
		if e.Label != "" {
			byLabel[e.Label] = e
		}
	}

	if len(serving) != 1 || serving[0].Weight != 100 {
		return nil, fmt.Errorf(
			"the traffic is split, so there is no single side to come back from; " +
				"resolve it with `evolve-deploy traffic <config> --to <label>`")
	}
	from := serving[0]
	if from.Label == "" {
		return nil, fmt.Errorf("all the traffic goes to an entry with no label")
	}

	labels := t.Strategy.Labels
	if len(labels) != 2 {
		return nil, fmt.Errorf("strategy.labels needs exactly two names, got %d", len(labels))
	}
	idx := slices.Index(labels, from.Label)
	if idx < 0 {
		return nil, fmt.Errorf("the label carrying all the traffic is %q, which is not one of %s",
			from.Label, strings.Join(labels, " or "))
	}

	to, ok := byLabel[labels[1-idx]]
	if !ok || to.Revision == "" {
		return nil, fmt.Errorf(
			"%s has never served anything, so there is nothing to go back to", labels[1-idx])
	}
	if to.Revision == from.Revision {
		return nil, fmt.Errorf(
			"both sides point at %s, so going back would change nothing", from.Revision)
	}

	return &rollbackStep{target: t, from: from, to: to}, nil
}

// runRollback says what it is about to do, then does it.
//
// No confirmation prompt, for the same reason traffic --to has none: a tool that
// asks during an outage is a tool in the way. What it does instead is print the
// versions first, so the line above the move is the record of what was traded
// for what.
func runRollback(
	ctx context.Context,
	out io.Writer,
	r target.Rollout,
	steps []rollbackStep,
) error {
	label := steps[0].to.Label
	fmt.Fprintf(out, "Rolling back to %s on %d target(s):\n", label, len(steps))
	fmt.Fprint(out, describeSteps(steps))
	fmt.Fprintln(out)

	var problems []string
	for _, s := range steps {
		// Printed before rather than after: the previous version is normally
		// stopped, so this call starts a container and waits for it, and a line
		// that appears only once it returns is a line that appears during the
		// silence it was meant to explain.
		fmt.Fprintf(out, "  %-40s starting %s\n", s.target.Label(), s.to.Revision)

		if err := r.Point(ctx, s.target, label); err != nil {
			fmt.Fprintf(out, "  %-40s failed: %v\n", s.target.Label(), err)
			problems = append(problems, fmt.Sprintf("%s: %v", s.target.Label(), err))
			continue
		}
		fmt.Fprintf(out, "  %-40s now on %s %s\n",
			s.target.Label(), label, orNone(s.to.Version))

		// The side just rolled away from is the one that was wrong, and it goes
		// on costing money until something switches it off. Point starts what it
		// is about to serve; this is the other half of that.
		tidy(ctx, out, r, s.target)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%d target(s) did not move:\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}

	// The same caveat traffic --to carries, and it matters more here: whoever
	// runs this is undoing a release, and the half of a release that is not
	// traffic is not undone by moving traffic.
	fmt.Fprint(out,
		"\nThis moved traffic only. Anything published per side — a Hive target,\n"+
			"a schema registry — still describes the version that was serving before.\n")
	return nil
}

// describeSteps renders the moves as a block, for the line before and for every
// refusal above. Whoever is told no needs to see what was actually there.
func describeSteps(steps []rollbackStep) string {
	var b strings.Builder
	for _, s := range steps {
		fmt.Fprintf(&b, "  %-40s %s %s -> %s %s\n", s.target.Label(),
			s.from.Label, orNone(s.from.Version),
			s.to.Label, orNone(s.to.Version))
	}
	return b.String()
}

func init() {
	RootCmd.AddCommand(rollbackCmd)
}
