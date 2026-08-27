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

var flagPruneDryRun bool

var pruneCmd = &cobra.Command{
	Use:   "prune <config>",
	Short: "Remove old revisions that nothing needs any more",
	Long: `Cloud Run keeps every revision it has ever been given. Nothing expires, so a
service deployed twice a day accumulates revisions until it meets a quota, and
the ones behind the two live sides are dead weight that no longer describes
anything running.

Three things are never removed, read off the platform at the moment of the sweep
rather than from any release:

  - the revision serving traffic, and anything else the traffic block names
  - the revision recorded as the side to roll back to
  - anything younger than the retention window

That second one is why this is a command rather than a shell loop. A blue-green
switch takes the tag off the side it retires, so from the outside the rollback
target looks exactly like the hundreds of revisions behind it — deleting it would
throw away the only thing a rollback can reach.

The retention window is 90 days and is currently hardcoded. It will become a
setting in the deploy config; until then, a revision younger than that is kept
whatever else is true of it.

Deleting a revision cannot be undone. Use --dry-run to see what would go.

The first sweep of a long-lived service is slow: each revision is removed and
waited for, and a service with hundreds of them takes a while.`,
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
		pruner, ok := driver.(target.Pruner)
		if !ok {
			return fmt.Errorf(
				"%s keeps no stack of revisions to sweep, so there is nothing here to "+
					"prune", driver.Name())
		}
		if err := driver.Verify(ctx); err != nil {
			return err
		}

		targets := prunableTargets(f, pruner)
		if len(targets) == 0 {
			return fmt.Errorf("%s has no target that holds revisions", f.Path)
		}
		return prune(ctx, out, pruner, targets, flagPruneDryRun)
	},
}

// prunableTargets is every target in the file whose revisions this can sweep.
//
// Not only the blue-green ones. A service deployed straight over itself
// accumulates revisions at exactly the same rate, and it is the accumulation
// this is for.
func prunableTargets(f *config.File, p target.Pruner) []*config.Target {
	var out []*config.Target
	for _, name := range f.ServiceNames() {
		c := f.Services[name]
		if c == nil {
			continue
		}
		for _, t := range c.Targets {
			if p.Prunable(t.Type) {
				out = append(out, t)
			}
		}
	}
	return out
}

// prune sweeps each target and says what it found.
//
// No confirmation prompt, for the same reason `traffic --to` has none: it says
// what it is about to do before doing it. What it does have that the others do
// not is --dry-run, because this is the one command whose mistakes cannot be
// undone by running the opposite of it.
func prune(
	ctx context.Context,
	out io.Writer,
	p target.Pruner,
	targets []*config.Target,
	dryRun bool,
) error {
	if dryRun {
		fmt.Fprintf(out, "Dry run: nothing will be removed.\n\n")
	}

	var (
		problems []string
		removed  int
	)
	for i, t := range targets {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "%-12s%s\n", t.Service, t.Label())

		looked, err := p.Prune(ctx, t, dryRun)
		if err != nil {
			fmt.Fprintf(out, "  could not be swept: %v\n", err)
			problems = append(problems, fmt.Sprintf("%s: %v", t.Label(), err))
			continue
		}

		var gone, kept int
		for _, r := range looked {
			if r.Removed() {
				gone++
				continue
			}
			kept++
			// Only the kept ones are listed, and only the reason they were kept.
			// A sweep of eight hundred revisions that printed each one would bury
			// the three lines worth reading, and those three are exactly the
			// question someone runs this to check.
			fmt.Fprintf(out, "  %-32s %-14s %s\n",
				r.Revision, ageOf(r), r.Keep)
		}
		removed += gone

		verb := "removed"
		if dryRun {
			verb = "would be removed"
		}
		fmt.Fprintf(out, "  %d kept, %d %s\n", kept, gone, verb)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%d target(s) could not be swept:\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}
	if removed == 0 && !dryRun {
		fmt.Fprint(out, "\nNothing was old enough to remove.\n")
	}
	return nil
}

// ageOf renders a revision's age in days, which is the only precision worth
// printing next to a ninety-day window.
func ageOf(p target.Pruned) string {
	if p.Age == 0 {
		return ""
	}
	return fmt.Sprintf("%d day(s) old", int(p.Age.Hours()/24))
}

func init() {
	pruneCmd.Flags().BoolVar(&flagPruneDryRun, "dry-run", false,
		"list what would be removed without removing anything")
	RootCmd.AddCommand(pruneCmd)
}
