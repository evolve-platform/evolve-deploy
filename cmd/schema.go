package cmd

import (
	"github.com/spf13/cobra"

	"github.com/evolve-platform/evolve-deploy/internal/schema"
)

var schemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Print the JSON Schema this version checks a config file against",
	Long: `Print the JSON Schema this version checks a config file against.

It is the same document a release publishes at
https://deploy.evolve-platform.com/schema/v<version>.json, and the one built
into this binary, so it is the right one for an editor whatever was published
since. Point an editor at the published one with a first line of:

  # yaml-language-server: $schema=https://deploy.evolve-platform.com/schema/latest.json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := cmd.OutOrStdout().Write(schema.Document(schema.URL(version)))
		return err
	},
}

func init() {
	RootCmd.AddCommand(schemaCmd)
}
