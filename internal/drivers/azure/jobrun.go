package azure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// jobRun is what a `uses: job` writes, worked out from one read of the job.
type jobRun struct {
	// run is the template the execution starts from: the release's image, and
	// the hook's command where it gave one.
	run []*armappcontainers.Container
	// keep is what the job is left with afterwards: the release's image and
	// the command Terraform declared. Only different from run when the hook
	// gave a command.
	keep []*armappcontainers.Container
	// write is false when the job already is run, which is the second time a
	// release is applied.
	write bool
}

// CheckJob reads the job and works out the write without making it.
func (d *Driver) CheckJob(ctx context.Context, j hooks.Job) error {
	_, err := d.prepareJobRun(ctx, j)
	return err
}

// RunJob starts one execution of a Container Apps job on the release's version
// and waits for it to finish.
//
// The image is written onto the job rather than handed to the execution. An
// execution can carry a template of its own, but only a cut-down one —
// image, command, env and resources — and whether the volume mounts and probes
// Terraform declared survive a start that overrides the containers is not
// something the API promises. Writing the job keeps every one of them by
// construction, and leaves the job showing the version it last ran.
//
// A command from the hook is the exception to leaving it there: it is for this
// run, so the job gets Terraform's back once the run is over, whatever the
// outcome. Otherwise the next run without a command would run the last one's.
func (d *Driver) RunJob(ctx context.Context, j hooks.Job, out io.Writer) (err error) {
	plan, err := d.prepareJobRun(ctx, j)
	if err != nil {
		return err
	}

	if plan.write {
		if err := d.patchJob(ctx, j.Name, plan.run); err != nil {
			return fmt.Errorf("container app job %s: %w", j.Name, err)
		}
	}
	if len(j.Command) > 0 {
		defer func() {
			// Not the caller's context: a run cancelled halfway is exactly when
			// the job most needs its own command back.
			restore := context.WithoutCancel(ctx)
			if rbErr := d.patchJob(restore, j.Name, plan.keep); rbErr != nil {
				err = errors.Join(err, fmt.Errorf(
					"container app job %s still has the command %q on it, "+
						"and the next run without one will run that: %w",
					j.Name, j.Command, rbErr))
			}
		}()
	}

	return d.startJob(ctx, j.Name, out)
}

func (d *Driver) prepareJobRun(ctx context.Context, j hooks.Job) (*jobRun, error) {
	if err := hooks.ErrAWSOnly(j); err != nil {
		return nil, err
	}
	got, err := d.jobs.Get(ctx, d.file.Cloud.ResourceGroup, j.Name, nil)
	if err != nil {
		return nil, fmt.Errorf("container app job %s: %w", j.Name, err)
	}
	job := got.Job
	if job.Properties == nil || job.Properties.Template == nil {
		return nil, fmt.Errorf("container app job %s has no template", j.Name)
	}

	current := job.Properties.Template.Containers
	name, err := target.PickContainer(containerNames(current), j.Container, appContainer)
	if err != nil {
		return nil, fmt.Errorf("container app job %s: %w", j.Name, err)
	}

	// The environment is left alone: a job a hook runs is not a target, and
	// what it reads is whatever Terraform gave it.
	run, from, err := nextContainers(current, name, j.Version, nil, false, j.Command)
	if err != nil {
		return nil, fmt.Errorf("container app job %s: %w", j.Name, err)
	}
	keep, _, err := nextContainers(current, name, j.Version, nil, false, nil)
	if err != nil {
		return nil, fmt.Errorf("container app job %s: %w", j.Name, err)
	}

	// Any command is a write, even one equal to what is there: the args go
	// with it, and the ones Terraform declared would extend it otherwise.
	return &jobRun{run: run, keep: keep, write: from != j.Version || len(j.Command) > 0}, nil
}

// startJob starts an execution and waits for it to stop.
//
// There is no deadline here. The job has its own — the replica timeout
// Terraform set — and Container Apps fails the execution when it passes, so a
// second one here could only be a guess at the same number, and a wrong one
// would walk away from a migration that is still running.
func (d *Driver) startJob(ctx context.Context, name string, out io.Writer) error {
	timer := logging.Start("start container app job", "name", name)
	poller, err := d.jobs.BeginStart(ctx, d.file.Cloud.ResourceGroup, name, nil)
	if err != nil {
		return fmt.Errorf("container app job %s: start: %w", name, err)
	}
	started, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("container app job %s: start: %w", name, err)
	}
	if started.Name == nil {
		return fmt.Errorf("container app job %s started without naming the execution", name)
	}
	exec := *started.Name
	fmt.Fprintf(out, "started execution %s\n", exec)

	begun := time.Now()
	for {
		got, err := d.executions.JobExecution(ctx, d.file.Cloud.ResourceGroup, name, exec, nil)
		if err != nil {
			return fmt.Errorf("container app job %s: execution %s: %w", name, exec, err)
		}
		var status armappcontainers.JobExecutionRunningState
		if p := got.Properties; p != nil && p.Status != nil {
			status = *p.Status
		}

		switch status {
		case armappcontainers.JobExecutionRunningStateSucceeded:
			timer.Done("execution", exec)
			fmt.Fprintf(out, "execution %s succeeded after %s\n",
				exec, time.Since(begun).Round(time.Second))
			return nil
		case armappcontainers.JobExecutionRunningStateFailed,
			armappcontainers.JobExecutionRunningStateStopped,
			armappcontainers.JobExecutionRunningStateDegraded:
			// What the container printed is in Log Analytics rather than here:
			// ARM has no call that returns it. The execution name is what
			// finds it there.
			return fmt.Errorf("container app job %s: execution %s %s after %s — "+
				"its output is in the job's execution history in resource group %s",
				name, exec, status, time.Since(begun).Round(time.Second),
				d.file.Cloud.ResourceGroup)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("container app job %s: gave up waiting for execution %s, "+
				"which may still be running: %w", name, exec, ctx.Err())
		case <-time.After(d.poll):
		}
	}
}
