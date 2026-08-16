// Package cmd wires the command line.
package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/drivers/aws"
	"github.com/evolve-platform/evolve-deploy/internal/drivers/azure"
	"github.com/evolve-platform/evolve-deploy/internal/drivers/gcp"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

var (
	// version is set by goreleaser.
	version = "dev"
	commit  = ""
)

// SetVersion is called from main so the build stamps land here.
func SetVersion(v, c string) { version, commit = v, c }

var (
	flagEnv     string
	flagDir     string
	flagSet     []string
	flagOnly    []string
	flagWorkers int
	flagVerbose bool
)

// RootCmd is the entry point.
var RootCmd = &cobra.Command{
	Use:   "evolve-deploy",
	Short: "Stateless deployments to AWS, GCP, Azure and Kubernetes",
	Long: `evolve-deploy reads a config file, compares it against what is actually
running, and rolls out the difference.

There is no state file and no lock: the cloud already knows what it runs.
Running it twice does nothing the second time.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	p := RootCmd.PersistentFlags()
	p.StringVarP(&flagEnv, "env", "e", "",
		"override the environment name used for ${env} and {{.env}} "+
			"(default: the config filename without its extension)")
	p.StringVar(&flagDir, "dir", ".", "working directory for hooks")
	p.StringSliceVar(&flagOnly, "only", nil, "limit to these services")
	p.StringSliceVar(&flagSet, "set", nil, "override a version, as name=version (repeatable)")
	p.IntVar(&flagWorkers, "workers", 16, "how many services to roll out at once")
	p.BoolVarP(&flagVerbose, "verbose", "v", false,
		"log every API call and every poll while waiting, to stderr")

	// Logging is installed before any command runs so that --verbose applies to
	// config loading too, not only to what happens after it.
	RootCmd.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		logging.Setup(cmd.ErrOrStderr(), flagVerbose)
	}

	RootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version",
	RunE: func(cmd *cobra.Command, args []string) error {
		if commit != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "evolve-deploy %s (%s)\n", version, commit)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "evolve-deploy %s\n", version)
		return nil
	},
}

// loadConfig reads the config file named on the command line.
//
// The path is the only way to say which environment is being deployed, so there
// is nothing that can contradict it. The environment name — what ${env} and
// {{.env}} expand to — comes from the filename by default, which keeps the two
// in step: deploy/tst.yaml is the tst environment. A repository that keeps one
// file per cloud (deploy/azure-tst.yaml) can set the name explicitly with --env,
// and even then it only changes the substitution, never which file is read.
func loadConfig(path string) (*config.File, error) {
	env := flagEnv
	if env == "" {
		env = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}

	f, err := config.Load(path, env)
	if err != nil {
		return nil, err
	}

	if err := f.SelectServices(flagOnly); err != nil {
		return nil, err
	}

	overrides, err := parseSet(flagSet)
	if err != nil {
		return nil, err
	}
	if err := f.SetVersions(overrides); err != nil {
		return nil, err
	}
	return f, nil
}

// parseSet reads --set name=version. In CI this is how tst gets its versions:
// the pipeline already knows which services it just built and at which sha,
// so nothing has to be committed for a test deploy.
func parseSet(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range pairs {
		name, version, ok := strings.Cut(pair, "=")
		if !ok || name == "" || version == "" {
			return nil, fmt.Errorf("--set %q is not name=version", pair)
		}
		out[name] = version
	}
	return out, nil
}

// newDriver picks the implementation for the cloud in the config.
func newDriver(ctx context.Context, f *config.File) (target.Driver, error) {
	switch f.Cloud.Provider {
	case config.CloudAWS:
		return aws.New(ctx, f)
	case config.CloudAzure:
		return azure.New(ctx, f)
	case config.CloudGCP:
		return gcp.New(ctx, f)
	default:
		return nil, &target.ErrNotImplemented{Cloud: string(f.Cloud.Provider)}
	}
}
