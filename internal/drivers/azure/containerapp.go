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
const readyTimeout = 10 * time.Minute

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

	if err := d.patchApp(ctx, ch.Target.Name, p.containers); err != nil {
		if rbErr := d.patchApp(ctx, ch.Target.Name, p.previous); rbErr != nil {
			return errors.Join(
				fmt.Errorf("rolling back to %s also failed", orNone(ch.FromVersion)),
				err, rbErr)
		}
		return fmt.Errorf("rolled back to %s: %w", orNone(ch.FromVersion), err)
	}
	return nil
}

func (d *Driver) revertApp(ctx context.Context, ch *target.Change) error {
	return d.patchApp(ctx, ch.Target.Name, ch.Payload.(*appPayload).previous)
}

// patchApp sends only the template.
//
// A merge patch rather than a full PUT, and deliberately so: Get never returns
// secret *values*, so writing the whole resource back would blank every secret
// Terraform declared. Sending just the template leaves configuration, ingress
// and secrets exactly as they are.
func (d *Driver) patchApp(ctx context.Context, name string, containers []*armappcontainers.Container) error {
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

	return d.waitReady(ctx, name)
}

// waitReady waits until the revision that was just created is the one serving.
//
// The long-running operation above completes when the revision is provisioned,
// which says nothing about whether the container started. Container Apps only
// promotes a revision to ready once its probes pass, so comparing the latest
// revision against the latest *ready* revision is the real health check.
func (d *Driver) waitReady(ctx context.Context, name string) error {
	started := time.Now()
	deadline := started.Add(readyTimeout)
	var lastState string

	slog.Debug("waiting for the revision to become ready",
		"name", name, "timeout", readyTimeout)

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

		if time.Now().After(deadline) {
			return fmt.Errorf(
				"%s: revision %s did not become ready within %s (ready revision is %s, state %s)",
				name, latest, readyTimeout, orNone(ready), lastState)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
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
