package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/evolve-platform/evolve-deploy/cmd"
)

// Set by goreleaser.
var (
	version = "dev"
	commit  = ""
)

func main() {
	cmd.SetVersion(version, commit)

	// A deploy that is interrupted halfway is worse than one that is not
	// started, so cancellation is propagated rather than the process being
	// killed outright: the drivers stop waiting, but a rollout already handed
	// to the cloud keeps going and can be picked up by the next run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cmd.RootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "\nerror: "+err.Error())
		os.Exit(1)
	}
}
