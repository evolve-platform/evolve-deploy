package cmd

import (
	"github.com/spf13/cobra"

	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/plan"
	"github.com/evolve-platform/evolve-deploy/internal/ui"
)

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

		return plan.Apply(ctx, p, plan.Options{
			Driver:      driver,
			Hooks:       &hooks.Runner{Dir: flagDir, Out: out, Verbose: flagVerbose},
			Out:         out,
			Concurrency: flagWorkers,
			Width:       ui.Width(p),
		})
	},
}

func init() {
	RootCmd.AddCommand(applyCmd)
}
