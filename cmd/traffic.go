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

With --to it puts one label on 100% and the other on 0, in one write, waiting
for nothing. That is the manual rollback for a release that went green but still
was not right, and the way out of a split.

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

		targets := routableTargets(f, rollout)
		if len(targets) == 0 {
			return fmt.Errorf("%s has no blue-green service that carries traffic", f.Path)
		}

		if flagTo == "" {
			return showTraffic(ctx, out, rollout, targets)
		}
		return moveTraffic(ctx, out, rollout, targets, flagTo)
	},
}

// routableTargets is every target in the file that carries traffic by label,
// in a stable order.
func routableTargets(f *config.File, r target.Rollout) []*config.Target {
	var out []*config.Target
	for _, name := range f.ServiceNames() {
		c := f.Services[name]
		if c == nil || !c.Strategy.IsBlueGreen() {
			continue
		}
		for _, t := range c.Targets {
			if r.Routable(t.Type) {
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
		if err := r.Point(ctx, t, label); err != nil {
			fmt.Fprintf(out, "  %-40s failed: %v\n", t.Label(), err)
			problems = append(problems, fmt.Sprintf("%s: %v", t.Label(), err))
			continue
		}
		fmt.Fprintf(out, "  %-40s now on %s\n", t.Label(), label)
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
