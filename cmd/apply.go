package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/plan"
	"github.com/evolve-platform/evolve-deploy/internal/ui"
)

var flagAllowEnvRemoval bool

var applyCmd = &cobra.Command{
	Use:   "apply <config>",
	Short: "Roll out the versions named in a config file",
	Long: `Compares the config against what is running and deploys the difference.

Nothing is touched until the whole plan resolves: every reference is looked up
and every image checked first, so a typo in one service cannot leave half a
release deployed.`,
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

		vars, err := hookVars(flagVar)
		if err != nil {
			return err
		}

		p, err := plan.Build(ctx, f, driver, vars)
		if err != nil {
			return err
		}

		ui.RenderPlan(out, p)
		if p.Empty() {
			return nil
		}

		if removals := p.EnvRemovals(); len(removals) > 0 && !flagAllowEnvRemoval {
			// The guidance goes to the output rather than into the error, so the
			// error itself stays a single sentence and the explanation stays
			// readable.
			fmt.Fprintf(out, "\nWould delete %d environment variable(s):\n  - %s\n\n",
				len(removals), strings.Join(removals, "\n  - "))
			fmt.Fprint(out,
				"The deploy config only ever sets variables, so these are gone from what\n"+
					"Terraform declares. Put them back there if that was not intended.\n\n")
			return fmt.Errorf(
				"refusing to delete %d environment variable(s); pass --allow-env-removal if you meant it",
				len(removals))
		}

		return plan.Apply(ctx, p, plan.Options{
			Driver:      driver,
			Hooks:       &hooks.Runner{Dir: flagDir, Out: out},
			Out:         out,
			Concurrency: flagWorkers,
			Width:       ui.Width(p),
			Vars:        vars,
		})
	},
}

func init() {
	applyCmd.Flags().BoolVar(&flagAllowEnvRemoval, "allow-env-removal", false,
		"permit removing environment variables that the config does not declare")
	RootCmd.AddCommand(applyCmd)
}
