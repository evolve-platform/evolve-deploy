package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

var flagTo string

var trafficCmd = &cobra.Command{
	Use:   "traffic <config>",
	Short: "Show which side is serving, or move all the traffic to one",
	Long: `Without --to this is read-only: it prints, per blue-green service, which
revision each label points at and what share of the traffic it gets. It is the
answer to "what is actually running".

With --to it puts one label on 100% and the other on 0. The side that is not
serving is normally stopped — it keeps its label, not its replicas — so the
revision is started and waited for before the weights move. That is the way out
of a split, and the way onto a side by name.

To undo a release you usually want "evolve-deploy rollback", which is this
without having to know which label to name, and which refuses when the targets
do not agree on the answer.

It reads the traffic block directly rather than going through the checks apply
uses, because the state this has to repair is exactly the state those checks
refuse to interpret.`,
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

		targets := pointableTargets(f, rollout)
		if len(targets) == 0 {
			return noSidesToMove(f, driver.Name())
		}

		if flagTo == "" {
			return showTraffic(ctx, out, rollout, targets)
		}
		return moveTraffic(ctx, out, rollout, targets, flagTo)
	},
}

// pointableTargets is every target in the file whose traffic these commands can
// move to a side by name, in a stable order.
//
// Deliberately Pointable and not Routable. An ECS service carries traffic and a
// release does move it, but ECS owns the sides and takes the old one away — so
// there is nothing here for `--to` to name, and listing it would offer
// something that cannot be done.
func pointableTargets(f *config.File, r target.Rollout) []*config.Target {
	var out []*config.Target
	for _, name := range f.ServiceNames() {
		c := f.Services[name]
		if c == nil || !c.Strategy.IsBlueGreen() {
			continue
		}
		for _, t := range c.Targets {
			if r.Pointable(t.Type) {
				out = append(out, t)
			}
		}
	}
	return out
}

func showTraffic(
	ctx context.Context,
	out io.Writer,
	r target.Rollout,
	targets []*config.Target,
) error {
	var problems []string

	for i, t := range targets {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "%-12s%s\n", t.Service, t.Label())

		entries, err := r.Traffic(ctx, t)
		if err != nil {
			fmt.Fprintf(out, "  could not be read: %v\n", err)
			problems = append(problems, fmt.Sprintf("%s: %v", t.Label(), err))
			continue
		}

		// Labels the config knows about but the platform has never been given
		// are worth showing as absent rather than left out: "green does not
		// exist yet" is the answer to a question someone is asking.
		seen := map[string]bool{}
		for _, e := range entries {
			seen[e.Label] = true

			revision := e.Revision
			if e.Latest {
				revision = "(latest)"
			}
			serving := ""
			if e.Weight == 100 {
				serving = "  <- serving"
			}
			fmt.Fprintf(out, "  %-8s %-40s %-10s %3d%%%s\n",
				orNone(e.Label), orNone(revision), orNone(e.Version), e.Weight, serving)
		}
		for _, label := range t.Strategy.Labels {
			if !seen[label] {
				fmt.Fprintf(out, "  %-8s (none)\n", label)
			}
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%d target(s) could not be read:\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// moveTraffic hands one label everything, everywhere it applies.
//
// No confirmation prompt. A tool that asks for confirmation during an outage is
// a tool in the way; what it does instead is say what it is about to do before
// doing it.
//
// Which is also why the per-target line is printed before Point rather than
// after: a stopped side has to be started first, and the wait for that is
// exactly the silence the line is there to explain.
func moveTraffic(
	ctx context.Context,
	out io.Writer,
	r target.Rollout,
	targets []*config.Target,
	label string,
) error {
	fmt.Fprintf(out, "Moving all traffic to %s on %d target(s):\n", label, len(targets))
	for _, t := range targets {
		fmt.Fprintf(out, "  %s\n", t.Label())
	}
	fmt.Fprintln(out)

	var problems []string
	for _, t := range targets {
		fmt.Fprintf(out, "  %-40s moving\n", t.Label())
		if err := r.Point(ctx, t, label); err != nil {
			fmt.Fprintf(out, "  %-40s failed: %v\n", t.Label(), err)
			problems = append(problems, fmt.Sprintf("%s: %v", t.Label(), err))
			continue
		}
		fmt.Fprintf(out, "  %-40s now on %s\n", t.Label(), label)
		tidy(ctx, out, r, t)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%d target(s) did not move:\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}

	// The schema half of a release is not this command's to undo, and finding
	// that out from a router serving a schema nobody implements is a bad way to
	// learn it.
	fmt.Fprint(out,
		"\nThis moved traffic only. Anything published per side — a Hive target,\n"+
			"a schema registry — still describes the version that was serving before.\n")
	return nil
}

func init() {
	trafficCmd.Flags().StringVar(&flagTo, "to", "",
		"put 100% of the traffic on this label, in one write")
	RootCmd.AddCommand(trafficCmd)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// tidy switches off what is no longer serving, and only complains.
//
// The same trade Settle makes after a deploy: the traffic moved, which is what
// was asked for, and failing the command over a revision that is still running
// would report a rollback as broken when it worked. It costs money until the
// next one, and the line says so.
func tidy(ctx context.Context, out io.Writer, r target.Rollout, t *config.Target) {
	if err := r.Tidy(ctx, t); err != nil {
		fmt.Fprintf(out, "  %-40s moved, but the cleanup failed: %v\n", t.Label(), err)
		fmt.Fprintf(out, "  %-40s (the side it came from is still running and still "+
			"costs money; the next release tidies it)\n", "")
	}
}

// noSidesToMove explains an empty target list.
//
// `traffic` reaches this on ECS, where there is no side to name; `rollback`
// does not, because it reverses the release instead — so the advice here is to
// use that rather than to reach for a redeploy.
func noSidesToMove(f *config.File, cloud string) error {
	if cloud != "aws" {
		return fmt.Errorf("%s has no blue-green service that carries traffic", f.Path)
	}
	return fmt.Errorf(
		"%s has no side that can be moved by name.\n"+
			"    ECS owns both target groups and swaps them itself, so a side is a role in\n"+
			"    one release rather than something standing that can be named.\n"+
			"    To go back, use `evolve-deploy rollback %s`: while the previous version is\n"+
			"    still running it puts the traffic back, and it says what to do instead when\n"+
			"    that window has closed.",
		f.Path, f.Path)
}
