package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// readyTimeout bounds the wait for a new revision to become ready. The
// long-running operation finishes as soon as the revision is provisioned, which
// is well before the container has started and passed its probes, so readiness
// is waited for separately.
//
// It is the backstop and not the mechanism. A revision that is never going to
// start is normally recognised as such within a few polls, from the revision
// itself; the deadline is for the case where Container Apps reports nothing
// conclusive at all.
const readyTimeout = 10 * time.Minute

// revertTimeout bounds the same wait when putting a template back, and is much
// shorter on purpose.
//
// A revert restores containers that were serving a moment ago, so it either
// comes up quickly or something is wrong that waiting will not fix. Giving it
// the full readyTimeout meant an app where nothing at all can start paid ten
// minutes for the deploy and ten more for the rollback — twenty minutes to
// arrive at a failure that was already certain. The deploy has failed either
// way; the only question left is how long that takes to say.
const revertTimeout = 2 * time.Minute

// pollInterval is how often the app and its newest revision are read while
// waiting.
const pollInterval = 5 * time.Second

// unhealthyStrikes is how many consecutive polls must call the revision
// unhealthy before the wait gives up on it.
//
// A container that crashes does not report one steady state: Container Apps
// restarts it, so the revision reads Degraded, then Processing, then Degraded
// again. One bad sighting is a container that has not finished starting; three
// in a row is one that is not going to. A revision that is merely slow reports
// Processing throughout and never takes a strike at all.
const unhealthyStrikes = 3

// crashLoopRestarts is the restart count at which a container that is still not
// ready is called doomed rather than slow. Container Apps restarts a container
// that exits immediately, and keeps doing it, so this is reached in well under
// a minute — and a process that has already died three times is not going to
// survive the fourth.
const crashLoopRestarts = 3

type appPayload struct {
	containers []*armappcontainers.Container
	// For rollback: the template exactly as it was before this change.
	previous []*armappcontainers.Container
}

// planApp works out the next revision of a Container App.
//
// Only the application container is touched. The reverse proxy beside it — and
// its proxy_env_vars — is copied through untouched, because that image and its
// settings belong to Terraform.
func (d *Driver) planApp(ctx context.Context, want *target.Desired) (*target.Change, error) {
	t := want.Target

	timer := logging.Start("get container app", "name", t.Name)
	got, err := d.apps.Get(ctx, d.file.Cloud.ResourceGroup, t.Name, nil)
	if err != nil {
		return nil, fmt.Errorf("container app %s: %w", t.Name, err)
	}
	timer.Done()
	app := got.ContainerApp
	if app.Properties == nil || app.Properties.Template == nil {
		return nil, fmt.Errorf("container app %s has no template", t.Name)
	}

	current := app.Properties.Template.Containers
	name, err := target.PickContainer(containerNames(current), t.Container, appContainer)
	if err != nil {
		return nil, fmt.Errorf("container app %s: %w", t.Name, err)
	}
	slog.Debug("container chosen", "app", t.Name, "container", name,
		"of", strings.Join(containerNames(current), ","))

	var declared []*armappcontainers.Secret
	if app.Properties.Configuration != nil {
		declared = app.Properties.Configuration.Secrets
	}
	if err := verifySecretRefs(want.Env, declared, "container app "+t.Name); err != nil {
		return nil, err
	}

	next, from, err := nextContainers(current, name, want.Version, want.Env, want.ManageEnv)
	if err != nil {
		return nil, err
	}

	added, changed, removed := diffContainers(current, next, name)
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
		Payload:     &appPayload{containers: next, previous: current},
	}, nil
}

func (d *Driver) applyApp(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*appPayload)

	if err := d.patchApp(ctx, ch.Target.Name, p.containers, readyTimeout); err != nil {
		if rbErr := d.patchApp(ctx, ch.Target.Name, p.previous, revertTimeout); rbErr != nil {
			return errors.Join(
				fmt.Errorf("rolling back to %s also failed", orNone(ch.FromVersion)),
				err, rbErr)
		}
		return fmt.Errorf("rolled back to %s: %w", orNone(ch.FromVersion), err)
	}
	return nil
}

func (d *Driver) revertApp(ctx context.Context, ch *target.Change) error {
	return d.patchApp(ctx, ch.Target.Name, ch.Payload.(*appPayload).previous, revertTimeout)
}

// patchApp sends only the template.
//
// A merge patch rather than a full PUT, and deliberately so: Get never returns
// secret *values*, so writing the whole resource back would blank every secret
// Terraform declared. Sending just the template leaves configuration, ingress
// and secrets exactly as they are.
func (d *Driver) patchApp(
	ctx context.Context,
	name string,
	containers []*armappcontainers.Container,
	timeout time.Duration,
) error {
	timer := logging.Start("patch container app", "name", name,
		"containers", len(containers))

	poller, err := d.apps.BeginUpdate(ctx, d.file.Cloud.ResourceGroup, name, armappcontainers.ContainerApp{
		Properties: &armappcontainers.ContainerAppProperties{
			Template: &armappcontainers.Template{Containers: containers},
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	timer.Done()

	return d.waitReady(ctx, name, timeout)
}

// waitReady waits until the revision that was just created is the one serving.
//
// The long-running operation above completes when the revision is provisioned,
// which says nothing about whether the container started. Container Apps only
// promotes a revision to ready once its probes pass, so comparing the latest
// revision against the latest *ready* revision is the real health check.
//
// That comparison can only ever say "not yet". The app resource carries no
// failure signal: its provisioning state stays Succeeded while the container
// inside it crash loops, and the ready revision simply lags behind. So on the
// app alone this loop has exactly one way to end badly, which is the deadline —
// ten minutes of waiting for something the platform decided in ten seconds. The
// revision underneath does carry that signal, and reading it is what turns the
// wait into a report.
func (d *Driver) waitReady(ctx context.Context, name string, timeout time.Duration) error {
	started := time.Now()
	deadline := started.Add(timeout)
	var (
		lastState  string
		lastReason string
		strikes    int
	)

	slog.Debug("waiting for the revision to become ready",
		"name", name, "timeout", timeout)

	for {
		got, err := d.apps.Get(ctx, d.file.Cloud.ResourceGroup, name, nil)
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", name, err)
		}
		props := got.Properties
		if props == nil {
			return fmt.Errorf("waiting for %s: no properties returned", name)
		}

		if props.ProvisioningState != nil {
			lastState = string(*props.ProvisioningState)
			switch *props.ProvisioningState {
			case armappcontainers.ContainerAppProvisioningStateFailed,
				armappcontainers.ContainerAppProvisioningStateCanceled:
				return fmt.Errorf("%s went to %s", name, lastState)
			}
		}

		latest := derefString(props.LatestRevisionName)
		ready := derefString(props.LatestReadyRevisionName)

		slog.Debug("poll", "name", name, "latest", latest,
			"ready", orNone(ready), "state", lastState,
			"elapsed", time.Since(started).Round(time.Second))

		if latest != "" && latest == ready {
			slog.Debug("revision is ready", "name", name, "revision", latest,
				"elapsed", time.Since(started).Round(time.Second))
			return nil
		}

		if latest != "" {
			reason, certain := d.inspectRevision(ctx, name, latest)
			switch {
			case reason == "":
				// Back to Processing after a bad reading: the container was
				// starting, not failing. The count starts over, so only a run
				// of consecutive failures ends the wait.
				strikes = 0
			case certain:
				return fmt.Errorf("%s: revision %s %s", name, latest, reason)
			default:
				strikes++
				lastReason = reason
				slog.Debug("revision looks unhealthy", "name", name,
					"revision", latest, "reason", reason,
					"strikes", strikes, "of", unhealthyStrikes)
				if strikes >= unhealthyStrikes {
					return fmt.Errorf("%s: revision %s %s", name, latest, reason)
				}
			}
		}

		if time.Now().After(deadline) {
			// Nothing was ever conclusive, so whatever the revision last said
			// about itself is the only clue there is. Better in the error than
			// behind a --verbose rerun that will not reproduce it.
			msg := fmt.Sprintf(
				"%s: revision %s did not become ready within %s (ready revision is %s, state %s)",
				name, latest, timeout, orNone(ready), lastState)
			if lastReason != "" {
				msg += "; the revision last reported that it " + lastReason
			}
			return errors.New(msg)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// inspectRevision reads the revision the app is trying to promote and says
// whether it can still become ready.
//
// It returns the reason it cannot, and whether that reason is certain. Certain
// means the platform has stopped trying, or a container has already died more
// often than a slow start can explain, and is acted on at once. An uncertain
// reason is only a strike — see unhealthyStrikes.
//
// A read that fails is never a verdict. The revision is created moments before
// this first runs, and a 404 while ARM catches up, or a throttled call, must
// not fail a deploy that is going fine. Those are logged and read as "nothing
// to report", which leaves readyTimeout as the backstop it is meant to be.
func (d *Driver) inspectRevision(ctx context.Context, app, revision string) (reason string, certain bool) {
	got, err := d.revisions.GetRevision(ctx, d.file.Cloud.ResourceGroup, app, revision, nil)
	if err != nil {
		slog.Debug("could not read the revision", "name", app,
			"revision", revision, "err", err)
		return "", false
	}

	reason, certain = classifyRevision(got.Properties)
	if reason == "" || certain {
		return reason, certain
	}

	// The revision says something is wrong but not what. The replicas do: a
	// container that has already restarted several times turns "Degraded" from
	// a suspicion into a fact, and either way its own state is the part worth
	// putting in front of whoever reads the failed deploy.
	if detail, crashing := d.inspectReplicas(ctx, app, revision); detail != "" {
		return reason + " — " + detail, crashing
	}
	return reason, false
}

// classifyRevision reads a revision the way the portal does: the platform's own
// provisioning verdict first, then what it says about the containers.
func classifyRevision(props *armappcontainers.RevisionProperties) (reason string, certain bool) {
	if props == nil {
		return "", false
	}

	// Provisioning is the platform's own verdict and it does not change its
	// mind: a failure here is an image that cannot be pulled, a quota, a
	// template it will not accept. ProvisioningError carries the sentence the
	// portal shows, which is usually the whole answer.
	if props.ProvisioningState != nil &&
		*props.ProvisioningState == armappcontainers.RevisionProvisioningStateFailed {
		if e := derefString(props.ProvisioningError); e != "" {
			return "failed to provision: " + e, true
		}
		return "failed to provision", true
	}

	// Running and health state are about the containers, and both report badly
	// while one is still on its way up — so they are suspicion, not verdict. A
	// revision that is merely slow sits in Processing and matches none of this.
	if props.RunningState != nil {
		switch *props.RunningState {
		case armappcontainers.RevisionRunningStateFailed,
			armappcontainers.RevisionRunningStateDegraded:
			return "is " + string(*props.RunningState), false
		}
	}
	if props.HealthState != nil &&
		*props.HealthState == armappcontainers.RevisionHealthStateUnhealthy {
		return "is Unhealthy", false
	}
	return "", false
}

// inspectReplicas describes what the containers in a revision are doing. Only
// called once the revision has already reported badly, so it costs nothing on
// a deploy that works.
func (d *Driver) inspectReplicas(ctx context.Context, app, revision string) (detail string, crashing bool) {
	got, err := d.replicas.ListReplicas(ctx, d.file.Cloud.ResourceGroup, app, revision, nil)
	if err != nil {
		slog.Debug("could not list the replicas", "name", app,
			"revision", revision, "err", err)
		return "", false
	}
	return describeReplicas(got.Value)
}

// describeReplicas summarises the containers that are not up, and says whether
// one of them is past saving.
//
// Restarts are what separate a slow start from a crash loop: a container that
// exits is restarted immediately and indefinitely, so a count in the low single
// digits is already a process that cannot stay up. RunningStateDetails is the
// string the portal shows next to it — an exit code, or the reason the image
// never ran at all.
func describeReplicas(replicas []*armappcontainers.Replica) (detail string, crashing bool) {
	var seen []string
	for _, r := range replicas {
		if r == nil || r.Properties == nil {
			continue
		}
		for _, c := range slices.Concat(r.Properties.InitContainers, r.Properties.Containers) {
			if c == nil || derefBool(c.Ready) {
				// A container that is up says nothing about why the revision is
				// not; the one next to it is the story.
				continue
			}

			restarts := derefInt32(c.RestartCount)
			if restarts >= crashLoopRestarts {
				crashing = true
			}

			line := derefString(c.Name)
			if c.RunningState != nil {
				line += " is " + string(*c.RunningState)
			}
			if d := derefString(c.RunningStateDetails); d != "" {
				line += " (" + d + ")"
			}
			if restarts > 0 {
				line += fmt.Sprintf(", %d restart(s)", restarts)
			}

			// Every replica of a broken revision reports the same thing, and
			// three copies of one sentence is not three times the information.
			if !slices.Contains(seen, line) {
				seen = append(seen, line)
			}
		}
	}
	sort.Strings(seen)
	return strings.Join(seen, "; "), crashing
}

// nextContainers copies the template and replaces the image tag of one
// container, and its environment when the config manages one. It returns the
// version that was running, read back from the image tag.
//
// With no environment in the config the existing one is carried over untouched:
// an empty list would delete every variable Terraform set.
func nextContainers(
	current []*armappcontainers.Container,
	name, version string,
	env []target.EnvVar,
	manageEnv bool,
) (next []*armappcontainers.Container, from string, err error) {
	next = make([]*armappcontainers.Container, 0, len(current))
	for _, c := range current {
		if c == nil {
			continue
		}
		if derefString(c.Name) != name {
			// A sidecar. Its image and environment are Terraform's.
			next = append(next, c)
			continue
		}

		img, err := image.Retag(derefString(c.Image), version)
		if err != nil {
			return nil, "", err
		}
		from = image.Tag(derefString(c.Image))

		replaced := *c
		replaced.Image = to.Ptr(img)
		if manageEnv {
			replaced.Env = renderEnv(env)
		}
		next = append(next, &replaced)
	}

	if findContainer(next, name) == nil {
		return nil, "", fmt.Errorf("container %q disappeared while building the revision", name)
	}
	return next, from, nil
}

func diffContainers(current, next []*armappcontainers.Container, name string) (added, changed, removed []string) {
	var have, want map[string]string
	if c := findContainer(current, name); c != nil {
		have = envFingerprint(c.Env)
	}
	if c := findContainer(next, name); c != nil {
		want = envFingerprint(c.Env)
	}

	for k, wv := range want {
		hv, ok := have[k]
		switch {
		case !ok:
			added = append(added, k)
		case hv != wv:
			changed = append(changed, k)
		}
	}
	for k := range have {
		if _, ok := want[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return added, changed, removed
}

func reason(from, to string) string {
	if from != to {
		return fmt.Sprintf("version %s -> %s", orNone(from), to)
	}
	return "environment changed"
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func sortedNames(m map[string]bool) []string {
	return slices.Sorted(maps.Keys(m))
}

func parseJSONMap(where, raw string) (map[string]string, error) {
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%s does not hold a JSON object of strings: %w", where, err)
	}
	return out, nil
}
