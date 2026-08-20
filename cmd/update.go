package cmd

import (
	"errors"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/evolve-platform/evolve-deploy/internal/gitlog"
	"github.com/evolve-platform/evolve-deploy/internal/update"
)

var flagUpdateCommits int

var updateCmd = &cobra.Command{
	Use:   "update <config>",
	Short: "Pick a version per service, from the ones that were actually built",
	Long: `Asks which version each service should run and writes the answers back.

This is how acc and prd get their versions: copy the previous environment's
file, run update, review the diff, merge. The list for a service holds one
entry per commit — with its subject line, newest first — and only the commits
whose artifact exists, so a version whose build job failed is never offered.

Commits are read from --dir. Nothing is written until every question has an
answer; leaving one as it is, or quitting, changes nothing.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		out := cmd.OutOrStdout()
		path := args[0]

		if !update.Interactive() {
			return errors.New(
				"update asks questions and needs a terminal — in a pipeline, " +
					"commit the versions or pass them to apply with --set")
		}

		f, err := loadConfig(path)
		if err != nil {
			return err
		}
		edit, err := update.Open(path)
		if err != nil {
			return err
		}
		driver, err := newDriver(ctx, f)
		if err != nil {
			return err
		}

		commits, err := gitlog.Recent(ctx, flagDir, flagUpdateCommits)
		if err != nil {
			return err
		}

		choices, err := update.Questions(ctx, f, driver, edit, commits)
		if err != nil {
			return err
		}

		// Services with nothing to offer are not asked about, so this is the
		// only place their reason gets said. Silence there would read as the
		// tool having skipped them for no reason.
		for _, c := range choices {
			if len(c.Options) == 0 {
				fmt.Fprintf(out, "%s: %s\n", c.Service, c.Note)
			}
		}

		picked, err := update.Ask(choices)
		if err != nil {
			if errors.Is(err, update.ErrAborted) {
				fmt.Fprintf(out, "Nothing was written to %s.\n", path)
				return nil
			}
			return err
		}
		if len(picked) == 0 {
			fmt.Fprintf(out, "Every service was left as it was; %s is unchanged.\n", path)
			return nil
		}

		names := make([]string, 0, len(picked))
		for name := range picked {
			names = append(names, name)
		}
		sort.Strings(names)

		width := 0
		for _, name := range names {
			width = max(width, len(name))
		}

		// Written in one pass at the end, after every answer is in: a run that
		// is interrupted half way through the questions must not leave a file
		// with three services promoted and eleven not.
		for _, name := range names {
			if err := edit.Set(name, picked[name]); err != nil {
				return err
			}
		}
		if err := edit.Save(); err != nil {
			return err
		}

		fmt.Fprintln(out)
		for _, name := range names {
			fmt.Fprintf(out, "%-*s  %s -> %s\n",
				width, name, f.Services[name].Version, picked[name])
		}
		fmt.Fprintf(out, "\nWrote %s. Check it with `evolve-deploy diff %s`.\n", path, path)
		return nil
	},
}

func init() {
	updateCmd.Flags().IntVar(&flagUpdateCommits, "commits", 30,
		"how many commits back to offer as versions")
	RootCmd.AddCommand(updateCmd)
}
