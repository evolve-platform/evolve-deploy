package azure

import (
	"context"
	"errors"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

type jobPayload struct {
	containers []*armappcontainers.Container
	previous   []*armappcontainers.Container
}

// planJob is planApp for a Container App Job.
//
// A job is the same image as its service, started with different arguments —
// four `evolve sync <job>` runs share one build. Those arguments live in the
// template and belong to Terraform, so only the tag and the environment change
// here.
func (d *Driver) planJob(ctx context.Context, want *target.Desired) (*target.Change, error) {
	t := want.Target

	got, err := d.jobs.Get(ctx, d.file.Cloud.ResourceGroup, t.Name, nil)
	if err != nil {
		return nil, fmt.Errorf("container app job %s: %w", t.Name, err)
	}
	job := got.Job
	if job.Properties == nil || job.Properties.Template == nil {
		return nil, fmt.Errorf("container app job %s has no template", t.Name)
	}

	current := job.Properties.Template.Containers
	name, err := target.PickContainer(containerNames(current), t.Container, appContainer)
	if err != nil {
		return nil, fmt.Errorf("container app job %s: %w", t.Name, err)
	}

	var declared []*armappcontainers.Secret
	if job.Properties.Configuration != nil {
		declared = job.Properties.Configuration.Secrets
	}
	if err := verifySecretRefs(want.Env, declared, "container app job "+t.Name); err != nil {
		return nil, err
	}

	next, from, err := nextContainers(current, name, want.Version, want.Env)
	if err != nil {
		return nil, err
	}

	// A job carries no traffic and so has no side of its own to write.
	added, changed, removed := diffContainers(current, next, name, nil)
	if len(added)+len(changed)+len(removed) == 0 && from == want.Version {
		return nil, nil
	}

	return &target.Change{
		Service:     want.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   want.Version,
		Reason:      reason(from, want.Version),
		EnvAdded:    added,
		EnvChanged:  changed,
		EnvRemoved:  removed,
		Payload:     &jobPayload{containers: next, previous: current},
	}, nil
}

func (d *Driver) applyJob(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*jobPayload)

	if err := d.patchJob(ctx, ch.Target.Name, p.containers); err != nil {
		if rbErr := d.patchJob(ctx, ch.Target.Name, p.previous); rbErr != nil {
			return errors.Join(
				fmt.Errorf("rolling back to %s also failed", orNone(ch.FromVersion)),
				err, rbErr)
		}
		return fmt.Errorf("rolled back to %s: %w", orNone(ch.FromVersion), err)
	}
	return nil
}

func (d *Driver) revertJob(ctx context.Context, ch *target.Change) error {
	return d.patchJob(ctx, ch.Target.Name, ch.Payload.(*jobPayload).previous)
}

// patchJob sends only the template, for the same reason patchApp does: Get
// never returns secret values, so writing the whole resource back would blank
// the secrets Terraform declared.
//
// There is no readiness to wait for. A job runs to completion when it is
// triggered — by a cron schedule or by hand — so once the definition is updated
// there is nothing further to observe. That also means a broken image is only
// discovered at the next run, not at deploy time.
func (d *Driver) patchJob(ctx context.Context, name string, containers []*armappcontainers.Container) error {
	timer := logging.Start("patch container app job", "name", name)

	poller, err := d.jobs.BeginUpdate(ctx, d.file.Cloud.ResourceGroup, name, armappcontainers.JobPatchProperties{
		Properties: &armappcontainers.JobPatchPropertiesProperties{
			Template: &armappcontainers.JobTemplate{Containers: containers},
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	res, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	timer.Done()

	if props := res.Properties; props != nil && props.ProvisioningState != nil {
		switch *props.ProvisioningState {
		case armappcontainers.JobProvisioningStateFailed,
			armappcontainers.JobProvisioningStateCanceled:
			return fmt.Errorf("%s went to %s", name, *props.ProvisioningState)
		}
	}
	return nil
}
