package gcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// jobRun is what a `uses: job` writes, worked out from one read of the job.
type jobRun struct {
	// run is the job the execution starts from: the release's image, and the
	// hook's command where it gave one.
	run *runpb.Job
	// keep is what the job is left with afterwards: the release's image and the
	// command Terraform declared. Only different from run when the hook gave a
	// command.
	keep *runpb.Job
	// write is false when the job already is run, which is the second time a
	// release is applied.
	write bool
}

// CheckJob reads the job and works out the write without making it.
func (d *Driver) CheckJob(ctx context.Context, j hooks.Job) error {
	_, err := d.prepareJobRun(ctx, j)
	return err
}

// RunJob runs a Cloud Run job once on the release's version and waits for the
// execution to finish.
//
// The image has to be written onto the job: RunJob's overrides take arguments,
// environment and a timeout, and neither an image nor an entry point. So a
// command from the hook is written onto the job as well, and Terraform's is put
// back once the run is over, whatever the outcome — otherwise the next run
// without a command would run the last one's.
func (d *Driver) RunJob(ctx context.Context, j hooks.Job, out io.Writer) (err error) {
	plan, err := d.prepareJobRun(ctx, j)
	if err != nil {
		return err
	}

	if plan.write {
		if err := d.updateJob(ctx, j.Name, plan.run); err != nil {
			return fmt.Errorf("cloud run job %s: %w", j.Name, err)
		}
	}
	if len(j.Command) > 0 {
		defer func() {
			// Not the caller's context: a run cancelled halfway is exactly when
			// the job most needs its own command back. Without the etag,
			// because the write above has moved the job on since it was read.
			restore := context.WithoutCancel(ctx)
			if rbErr := d.updateJob(restore, j.Name, stale(plan.keep)); rbErr != nil {
				err = errors.Join(err, fmt.Errorf(
					"cloud run job %s still has the command %q on it, "+
						"and the next run without one will run that: %w",
					j.Name, j.Command, rbErr))
			}
		}()
	}

	return d.executeJob(ctx, j.Name, out)
}

func (d *Driver) prepareJobRun(ctx context.Context, j hooks.Job) (*jobRun, error) {
	job, err := d.getJob(ctx, j.Name)
	if err != nil {
		return nil, err
	}

	name, err := target.PickContainer(containerNames(jobContainers(job)), j.Container, cloudRunContainer)
	if err != nil {
		return nil, fmt.Errorf("cloud run job %s: %w", j.Name, err)
	}

	// The environment is left alone: a job a hook runs is not a target, and
	// what it reads is whatever Terraform gave it.
	run, from, err := nextJob(job, name, j.Version, nil, false, j.Command)
	if err != nil {
		return nil, fmt.Errorf("cloud run job %s: %w", j.Name, err)
	}
	keep, _, err := nextJob(job, name, j.Version, nil, false, nil)
	if err != nil {
		return nil, fmt.Errorf("cloud run job %s: %w", j.Name, err)
	}

	// Any command is a write, even one equal to what is there: the args go
	// with it, and the ones Terraform declared would extend it otherwise.
	return &jobRun{run: run, keep: keep, write: from != j.Version || len(j.Command) > 0}, nil
}

// executeJob runs the job and waits for the execution to finish.
//
// The operation RunJob returns completes when the execution does, not when it
// starts, so waiting on it is the whole wait. There is no deadline of our own:
// the job's task timeout is Terraform's, Cloud Run fails the execution when it
// passes, and a shorter one here would walk away from a migration that is still
// running.
func (d *Driver) executeJob(ctx context.Context, name string, out io.Writer) error {
	timer := logging.Start("run cloud run job", "name", name)
	op, err := d.jobs.RunJob(ctx, &runpb.RunJobRequest{Name: d.jobName(name)})
	if err != nil {
		return fmt.Errorf("cloud run job %s: run: %w", name, err)
	}

	// The metadata is the execution as it was created, which is where its
	// name and the link to its logs are known before it has finished.
	var logs string
	if meta, err := op.Metadata(); err == nil && meta != nil {
		fmt.Fprintf(out, "started execution %s\n", shortName(meta.GetName()))
		logs = meta.GetLogUri()
	}

	begun := time.Now()
	done, err := op.Wait(ctx)
	took := time.Since(begun).Round(time.Second)
	if done != nil && done.GetLogUri() != "" {
		logs = done.GetLogUri()
	}
	where := ""
	if logs != "" {
		where = " — its logs: " + logs
	}

	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("cloud run job %s: gave up waiting after %s, "+
			"and the execution may still be running%s: %w", name, took, where, ctx.Err())
	case err != nil:
		return fmt.Errorf("cloud run job %s: execution failed after %s%s: %w",
			name, took, where, err)
	case done.GetFailedCount() > 0 || done.GetCancelledCount() > 0:
		return fmt.Errorf("cloud run job %s: execution %s finished after %s with "+
			"%d task(s) failed and %d cancelled%s", name, shortName(done.GetName()),
			took, done.GetFailedCount(), done.GetCancelledCount(), where)
	}

	timer.Done("execution", done.GetName())
	fmt.Fprintf(out, "execution %s succeeded after %s\n", shortName(done.GetName()), took)
	return nil
}
