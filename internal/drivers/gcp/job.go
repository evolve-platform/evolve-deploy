package gcp

import (
	"context"
	"errors"
	"fmt"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"

	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

type jobPayload struct {
	job      *runpb.Job
	previous *runpb.Job
}

// planJob is planService for a Cloud Run job.
//
// A job is the same image as the service beside it, started with a different
// entry point — four `evolve sync <job>` runs share one build. That entry point
// is Terraform's unless the config states one, in which case it travels with the
// version: a command names a path inside the image, so an image whose layout
// moved and a command that has not are a job that will not start, and the two
// have to land in one write.
func (d *Driver) planJob(ctx context.Context, want *target.Desired) (*target.Change, error) {
	t := want.Target

	job, err := d.getJob(ctx, t.Name)
	if err != nil {
		return nil, err
	}

	current := jobContainers(job)
	name, err := target.PickContainer(containerNames(current), t.Container, cloudRunContainer)
	if err != nil {
		return nil, fmt.Errorf("cloud run job %s: %w", t.Name, err)
	}

	next, from, err := nextJob(job, name, want.Version, want.Env, want.ManageEnv, t.Command)
	if err != nil {
		return nil, err
	}

	// A job carries no traffic and so has no side of its own to write, which is
	// also why PublicURL stays empty: there is no ingress to name.
	added, changed, removed := diffEnv(current, jobContainers(next), name, nil)
	command := diffCommand(current, jobContainers(next), name)
	if len(added)+len(changed)+len(removed)+len(command) == 0 && from == want.Version {
		return nil, nil
	}

	return &target.Change{
		Service:     want.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   want.Version,
		Reason: reason(from, want.Version,
			len(added)+len(changed)+len(removed) > 0, len(command) > 0),
		EnvAdded:   added,
		EnvChanged: changed,
		EnvRemoved: removed,
		Command:    command,
		Payload:    &jobPayload{job: next, previous: job},
	}, nil
}

func (d *Driver) applyJob(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*jobPayload)

	if err := d.updateJob(ctx, ch.Target.Name, p.job); err != nil {
		// Worth more here than beside a service. A revision that will not start
		// never takes traffic, so a failed service deploy keeps serving the old
		// version on its own; a job definition has nothing in front of it, and
		// what is left behind is what the 03:00 cron runs.
		if rbErr := d.updateJob(ctx, ch.Target.Name, stale(p.previous)); rbErr != nil {
			return errors.Join(
				fmt.Errorf("rolling back to %s also failed", target.VersionOrUnknown(ch.FromVersion)),
				err, rbErr)
		}
		return fmt.Errorf("rolled back to %s: %w", target.VersionOrUnknown(ch.FromVersion), err)
	}
	return nil
}

func (d *Driver) revertJob(ctx context.Context, ch *target.Change) error {
	return d.updateJob(ctx, ch.Target.Name, stale(ch.Payload.(*jobPayload).previous))
}

// getJob reads a job and checks the part every caller needs. A job holds its
// containers two messages down: the execution template describes one run, and
// the task template inside it describes the containers of one task.
func (d *Driver) getJob(ctx context.Context, name string) (*runpb.Job, error) {
	timer := logging.Start("get cloud run job", "name", name)
	job, err := d.jobs.GetJob(ctx, &runpb.GetJobRequest{Name: d.jobName(name)})
	if err != nil {
		return nil, fmt.Errorf("cloud run job %s: %w", name, err)
	}
	timer.Done()

	if job.GetTemplate().GetTemplate() == nil {
		return nil, fmt.Errorf("cloud run job %s has no template", name)
	}
	return job, nil
}

// updateJob writes the job back and waits for it to reconcile.
//
// The whole resource goes over the wire, where a service is written with a
// field mask that limits it to the template. That is not a choice: UpdateJob
// takes no mask, so a request carrying only the template would be a job with
// nothing else on it. What keeps Terraform's settings — parallelism, retries,
// timeout, the service account — is that this is the job that was read, with
// one container's image and environment changed and everything else untouched.
//
// There is no readiness to wait for beyond that. A job runs to completion when
// it is triggered, by a schedule or by hand, so once the definition is
// reconciled there is nothing further to observe — which also means a broken
// image is discovered at the next run rather than at deploy time.
func (d *Driver) updateJob(ctx context.Context, name string, job *runpb.Job) error {
	timer := logging.Start("update cloud run job", "name", name,
		"containers", len(jobContainers(job)), "etag", job.GetEtag() != "")

	op, err := d.jobs.UpdateJob(ctx, &runpb.UpdateJobRequest{Job: job})
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	done, err := op.Wait(ctx)
	if err != nil {
		return fmt.Errorf("waiting for %s: %w", name, err)
	}
	timer.Done("generation", done.GetGeneration())

	// The operation succeeding is not quite the same as the job being usable,
	// so the terminal condition is checked as well.
	if cond := done.GetTerminalCondition(); cond != nil &&
		cond.GetState() != runpb.Condition_CONDITION_SUCCEEDED &&
		cond.GetState() != runpb.Condition_STATE_UNSPECIFIED {
		return fmt.Errorf("%s is %s: %s", name, cond.GetState(), cond.GetMessage())
	}
	return nil
}

// nextJob copies the job and rewrites the application container inside its task
// template. Everything else on the job is carried through, because everything
// else on it is Terraform's.
func nextJob(
	current *runpb.Job,
	name, version string,
	env []target.EnvVar,
	manageEnv bool,
	command []string,
) (next *runpb.Job, from string, err error) {
	next = proto.Clone(current).(*runpb.Job)

	from, err = retag(jobContainers(next), name, version, env, manageEnv, command)
	if err != nil {
		return nil, "", err
	}
	return next, from, nil
}

// stale is a copy of a job with the etag dropped, for the writes that put a
// previous definition back.
//
// An etag makes a concurrent write fail instead of overwriting it, which is
// what the deploy wants. A rollback wants the opposite: the state has moved on
// since it was read — that is precisely why there is something to undo — and
// refusing to undo because of that would be the wrong way round.
func stale(job *runpb.Job) *runpb.Job {
	out := proto.Clone(job).(*runpb.Job)
	out.Etag = ""
	return out
}

func jobContainers(job *runpb.Job) []*runpb.Container {
	return job.GetTemplate().GetTemplate().GetContainers()
}
