package cmd

import (
	"github.com/spf13/cobra"

	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/plan"
	"github.com/evolve-platform/evolve-deploy/internal/ui"
)

var flagDiffExitCode bool

var diffCmd = &cobra.Command{
	Use:   "diff <config>",
	Short: "Show what apply would do, without doing it",
	Long: `Runs the same plan as apply and prints it.

This is what fills the hole left by dropping terraform plan: it resolves every
reference, checks every image, and shows which environment variables would
change and why — including a rollout caused by Terraform moving a base task
definition rather than by anything in this file.`,
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

		p, err := plan.Build(ctx, f, driver)
		if err != nil {
			return err
		}

		ui.RenderPlan(out, p)
		ui.Summary(out, p)

		// Print the hooks that would run, since they are part of what a deploy
		// does and are easy to forget.
		runner := &hooks.Runner{Dir: flagDir, Out: out, DryRun: true}
		for _, cp := range p.Services {
			if !cp.HasWork() {
				continue
			}
			vars := hooks.Vars{
				"version": cp.Service.Version,
				"name":    cp.Service.Name,
				"env":     cp.Env,
			}
			_ = runner.Run(ctx, "before", cp.Service.Before, vars)
			_ = runner.Run(ctx, "after", cp.Service.After, vars)
		}

		if flagDiffExitCode && !p.Empty() {
			// Lets a pipeline gate on drift: "is production what the file says?"
			cmd.SilenceUsage = true
			return errDrift
		}
		return nil
	},
}

// errDrift is returned by diff --exit-code when there is work to do.
var errDrift = &driftError{}

type driftError struct{}

func (*driftError) Error() string { return "there are changes to apply" }

func init() {
	diffCmd.Flags().BoolVar(&flagDiffExitCode, "exit-code", false,
		"exit non-zero when there is anything to apply")
	RootCmd.AddCommand(diffCmd)
}
