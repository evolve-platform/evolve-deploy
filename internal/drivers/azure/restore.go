package azure

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// restoreFrom is what a version ran with, as the platform still records it: the
// parts of the application container a release writes. Everything else in the
// template — resources, probes, the sidecar — is Terraform's and is kept as it
// is now, the same as a release keeps it.
type restoreFrom struct {
	env     []*armappcontainers.EnvironmentVar
	command []*string
	args    []*string
	// where names the record, for the line the rollback prints.
	where string
}

// RiderVersion reads the image tag a rider runs now.
func (d *Driver) RiderVersion(ctx context.Context, t *config.Target) (string, error) {
	current, name, err := d.riderContainers(ctx, t)
	if err != nil {
		return "", err
	}
	return image.Tag(derefString(findContainer(current, name).Image)), nil
}

// PlanRestore puts a worker or a job back on a version, from what the platform
// still holds of it.
//
// A worker's revisions outlive it: Single mode deactivates the one it replaces
// but keeps its template, so the newest revision on that version is exactly what
// ran. Its template is patched back rather than the revision activated — a
// revision activated in Single mode is not reliably the one that serves, and a
// patch goes through the same ready wait and revert as a release. A job has no
// revisions; its runs are the record, each holding the containers it started
// with. Where neither is left — the platform keeps a bounded number of both —
// only the tag goes back, and the Reason says the rest is today's.
func (d *Driver) PlanRestore(ctx context.Context, t *config.Target, version string) (*target.Change, error) {
	current, name, err := d.riderContainers(ctx, t)
	if err != nil {
		return nil, err
	}
	from := image.Tag(derefString(findContainer(current, name).Image))
	if from == version {
		return nil, nil
	}

	var source *restoreFrom
	if t.Type == config.TypeContainerJob {
		source, err = d.jobRunOn(ctx, t.Name, name, version)
	} else {
		source, err = d.revisionOn(ctx, t.Name, name, version)
	}
	if err != nil {
		return nil, err
	}

	var (
		next []*armappcontainers.Container
		why  string
	)
	if source == nil {
		next, _, err = nextContainers(current, name, version, nil, false, nil)
		why = fmt.Sprintf("version %s -> %s, image only: nothing recorded that version's "+
			"environment and command, so they stay as they are now", target.VersionOrUnknown(from), version)
	} else {
		next, err = restoredContainers(current, name, version, source)
		why = fmt.Sprintf("version %s -> %s, as %s ran it", target.VersionOrUnknown(from), version, source.where)
	}
	if err != nil {
		return nil, err
	}

	added, changed, removed := diffContainers(current, next, name, nil)
	ch := &target.Change{
		Service:     t.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   version,
		Reason:      why,
		EnvAdded:    added,
		EnvChanged:  changed,
		EnvRemoved:  removed,
		Command:     diffCommand(current, next, name),
	}
	if t.Type == config.TypeContainerJob {
		ch.Payload = &jobPayload{containers: next, previous: current}
	} else {
		ch.Payload = &appPayload{containers: next, previous: current}
	}
	return ch, nil
}

// riderContainers reads the template a rider runs now and picks its
// application container.
//
// Only the two rider shapes that have a record of past versions are restorable.
// A function app swaps slots and a routable app is moved by its label, so both
// are refused here and the caller tells the user what to run instead.
func (d *Driver) riderContainers(
	ctx context.Context,
	t *config.Target,
) ([]*armappcontainers.Container, string, error) {
	var current []*armappcontainers.Container

	switch {
	case t.Type == config.TypeContainerJob:
		got, err := d.jobs.Get(ctx, d.file.Cloud.ResourceGroup, t.Name, nil)
		if err != nil {
			return nil, "", fmt.Errorf("container app job %s: %w", t.Name, err)
		}
		if got.Properties == nil || got.Properties.Template == nil {
			return nil, "", fmt.Errorf("container app job %s has no template", t.Name)
		}
		current = got.Properties.Template.Containers
	case t.Type == config.TypeContainerApp && t.NoIngress():
		app, err := d.getApp(ctx, t.Name)
		if err != nil {
			return nil, "", err
		}
		if err := checkWorker(t.Name, app.Properties.Configuration); err != nil {
			return nil, "", err
		}
		current = app.Properties.Template.Containers
	default:
		return nil, "", &target.ErrNotImplemented{Cloud: "azure", Type: t.Type}
	}

	name, err := target.PickContainer(containerNames(current), t.Container, appContainer)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", t.Label(), err)
	}
	return current, name, nil
}

// revisionOn finds the newest revision of a worker whose application container
// ran version. Nil when none is left.
//
// A failed read is an error rather than "none left": falling back to the tag
// alone over a throttled call would restore less than was there to restore.
func (d *Driver) revisionOn(ctx context.Context, app, name, version string) (*restoreFrom, error) {
	var (
		best    *restoreFrom
		created time.Time
	)

	pager := d.revisions.NewListRevisionsPager(d.file.Cloud.ResourceGroup, app, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing the revisions of %s: %w", app, err)
		}
		for _, rev := range page.Value {
			if rev == nil || rev.Properties == nil || rev.Properties.Template == nil {
				continue
			}
			c := findContainer(rev.Properties.Template.Containers, name)
			if c == nil || image.Tag(derefString(c.Image)) != version {
				continue
			}
			at := derefTime(rev.Properties.CreatedTime)
			if best != nil && !at.After(created) {
				continue
			}
			best = &restoreFrom{
				env:     c.Env,
				command: c.Command,
				args:    c.Args,
				where:   "revision " + derefString(rev.Name),
			}
			created = at
		}
	}
	return best, nil
}

// jobRunOn finds the newest run of a job whose application container ran
// version. Nil when none is left, which is also the answer for a job that has
// not run since that version was released.
func (d *Driver) jobRunOn(ctx context.Context, job, name, version string) (*restoreFrom, error) {
	var (
		best    *restoreFrom
		started time.Time
	)

	pager := d.jobRuns.NewListPager(d.file.Cloud.ResourceGroup, job, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing the runs of %s: %w", job, err)
		}
		for _, run := range page.Value {
			if run == nil || run.Properties == nil || run.Properties.Template == nil {
				continue
			}
			var c *armappcontainers.JobExecutionContainer
			for _, rc := range run.Properties.Template.Containers {
				if rc != nil && derefString(rc.Name) == name {
					c = rc
				}
			}
			if c == nil || image.Tag(derefString(c.Image)) != version {
				continue
			}
			at := derefTime(run.Properties.StartTime)
			if best != nil && !at.After(started) {
				continue
			}
			best = &restoreFrom{
				env:     c.Env,
				command: c.Command,
				args:    c.Args,
				where:   "run " + derefString(run.Name),
			}
			started = at
		}
	}
	return best, nil
}

// restoredContainers is nextContainers with the environment and entry point
// taken from a record instead of from the config.
//
// The repository is the one on the template now, not the record's: the tool
// only ever moves the tag, and an image that moved registry since is Terraform's
// change to keep.
func restoredContainers(
	current []*armappcontainers.Container,
	name, version string,
	from *restoreFrom,
) ([]*armappcontainers.Container, error) {
	next := make([]*armappcontainers.Container, 0, len(current))
	for _, c := range current {
		if c == nil {
			continue
		}
		if derefString(c.Name) != name {
			next = append(next, c)
			continue
		}
		img, err := image.Retag(derefString(c.Image), version)
		if err != nil {
			return nil, err
		}
		replaced := *c
		replaced.Image = to.Ptr(img)
		replaced.Env = from.env
		replaced.Command = from.command
		replaced.Args = from.args
		next = append(next, &replaced)
	}
	return next, nil
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
