package azure

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"golang.org/x/sync/errgroup"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
	"github.com/evolve-platform/evolve-deploy/internal/tmpl"
)

// tagPages bounds how deep the tag list is read.
//
// Tags come back newest-pushed first, a hundred per page, and the versions being
// asked about are the last few dozen commits — so the first page nearly always
// settles it and the reading stops as soon as every candidate is accounted for.
// The cap is there for the registry that holds every tag of every service:
// something absent from the five hundred most recent pushes is not a version
// anyone is about to deploy.
const tagPages = 5

// artifactProbes bounds how many packages are checked at once, so a long
// version list does not arrive at blob storage as a burst.
const artifactProbes = 8

// Artifacts reports which of the given versions could be deployed to this
// target.
func (d *Driver) Artifacts(
	ctx context.Context, t *config.Target, versions []string,
) ([]string, error) {
	switch t.Type {
	case config.TypeContainerApp, config.TypeContainerJob:
		ref, err := d.currentImage(ctx, t)
		if err != nil {
			return nil, err
		}
		return d.imageVersions(ctx, ref, versions)
	case config.TypeFunctionApp:
		return d.packageVersions(ctx, t, versions)
	default:
		return nil, &target.ErrNotImplemented{Cloud: d.Name(), Type: t.Type}
	}
}

// currentImage reads the image the application container runs now, which is
// where the registry and repository come from. They are never in the config —
// only the tag is ever replaced — so the running resource is the only place to
// learn which repository to ask.
func (d *Driver) currentImage(ctx context.Context, t *config.Target) (string, error) {
	var (
		containers []*armappcontainers.Container
		err        error
	)
	switch t.Type {
	case config.TypeContainerApp:
		containers, err = d.appContainers(ctx, t.Name)
	default:
		containers, err = d.jobContainers(ctx, t.Name)
	}
	if err != nil {
		return "", err
	}

	name, err := target.PickContainer(containerNames(containers), t.Container, appContainer)
	if err != nil {
		return "", fmt.Errorf("%s: %w", t.Label(), err)
	}
	c := findContainer(containers, name)
	if c == nil {
		return "", fmt.Errorf("%s: container %q disappeared", t.Label(), name)
	}
	return derefString(c.Image), nil
}

func (d *Driver) appContainers(ctx context.Context, name string) ([]*armappcontainers.Container, error) {
	got, err := d.apps.Get(ctx, d.file.Cloud.ResourceGroup, name, nil)
	if err != nil {
		return nil, fmt.Errorf("container app %s: %w", name, err)
	}
	if got.Properties == nil || got.Properties.Template == nil {
		return nil, fmt.Errorf("container app %s has no template", name)
	}
	return got.Properties.Template.Containers, nil
}

func (d *Driver) jobContainers(ctx context.Context, name string) ([]*armappcontainers.Container, error) {
	got, err := d.jobs.Get(ctx, d.file.Cloud.ResourceGroup, name, nil)
	if err != nil {
		return nil, fmt.Errorf("container app job %s: %w", name, err)
	}
	if got.Properties == nil || got.Properties.Template == nil {
		return nil, fmt.Errorf("container app job %s has no template", name)
	}
	return got.Properties.Template.Containers, nil
}

// imageVersions asks the registry which of the tags exist.
//
// Container Apps pulls with the app's own managed identity, so the registry is
// reachable from the deploy without any extra configuration — but the identity
// running this tool needs AcrPull on it to read the tag list, which is a
// separate grant from anything a deploy does today.
func (d *Driver) imageVersions(ctx context.Context, ref string, versions []string) ([]string, error) {
	repo, _ := image.Split(ref)
	host, path, found := strings.Cut(repo, "/")
	if !found || !strings.Contains(host, ".azurecr.") {
		// Not an Azure Container Registry. Anything else — Docker Hub, GHCR —
		// is still deployable, so this is a missing list and not a failure.
		return nil, fmt.Errorf("%s: %w", ref, target.ErrArtifactsUnknown)
	}

	client, err := d.registryFor(host)
	if err != nil {
		return nil, err
	}

	timer := logging.Start("list tags", "registry", host, "repository", path)
	// Newest first, so the pages are read in the order that makes the wanted
	// tags turn up soonest.
	order := azcontainerregistry.ArtifactTagOrderByLastUpdatedOnDescending
	pager := client.NewListTagsPager(path, &azcontainerregistry.ClientListTagsOptions{
		OrderBy: &order,
	})

	want := map[string]bool{}
	for _, v := range versions {
		want[v] = true
	}
	have := map[string]bool{}

	for page := 0; page < tagPages && pager.More() && len(have) < len(want); page++ {
		got, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing tags of %s in %s: %w", path, host, err)
		}
		for _, tag := range got.Tags {
			if tag == nil {
				continue
			}
			if name := derefString(tag.Name); want[name] {
				have[name] = true
			}
		}
	}
	timer.Done("found", len(have), "of", len(want))

	return target.Present(versions, have), nil
}

// registryFor returns a client for one registry host, built once.
//
// A service is usually several targets sharing an image — a container app and
// four jobs — and each of those asks about the same registry. Building the
// client once means the token for it is fetched once too.
func (d *Driver) registryFor(host string) (*azcontainerregistry.Client, error) {
	d.registryMu.Lock()
	defer d.registryMu.Unlock()

	if client, ok := d.registries[host]; ok {
		return client, nil
	}
	client, err := azcontainerregistry.NewClient("https://"+host, d.cred, nil)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", host, err)
	}
	if d.registries == nil {
		d.registries = map[string]*azcontainerregistry.Client{}
	}
	d.registries[host] = client
	return client, nil
}

// packageVersions checks which function app packages exist, one read of the
// blob's properties per candidate.
//
// Listing the container would be one call rather than thirty, but the package
// URL is a full blob URL and the part before {{.version}} is not necessarily a
// prefix anyone may list — the deploy only ever needs to read one known blob. A
// miss is a 404 and costs nothing.
func (d *Driver) packageVersions(
	ctx context.Context, t *config.Target, versions []string,
) ([]string, error) {
	if t.Code == nil || t.Code.URL == "" {
		// A function app that runs a container carries no package, and which of
		// the two it is can only be seen on the resource. Saying so is better
		// than an empty list.
		return nil, fmt.Errorf("%s has no code.url: %w", t.Label(), target.ErrArtifactsUnknown)
	}

	found, err := probe(ctx, versions, func(gctx context.Context, version string) (bool, error) {
		url, err := tmpl.Render(t.Code.URL, map[string]string{
			"version": version,
			"name":    t.Name,
		})
		if err != nil {
			return false, fmt.Errorf("code.url: %w", err)
		}
		client, err := d.blobFor(url)
		if err != nil {
			return false, err
		}
		// Absent is the expected answer for most candidates, and blob storage
		// answers a missing blob and a forbidden one the same way when the
		// caller may not list. Treating both as absent is what makes this
		// usable without a listing grant; the cost is that a misconfigured URL
		// looks like "nothing was built", which the update command says out
		// loud when a service has no candidates — and --verbose says why.
		_, err = client.GetProperties(gctx, nil)
		if err != nil {
			slog.Debug("no package", "url", url, "err", err)
		}
		return err == nil, nil
	})
	if err != nil {
		return nil, err
	}
	return target.Present(versions, found), nil
}

// probe runs check for every version at once, bounded, and returns the ones it
// said yes to.
func probe(
	ctx context.Context,
	versions []string,
	check func(context.Context, string) (bool, error),
) (map[string]bool, error) {
	var (
		mu    sync.Mutex
		found = map[string]bool{}
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(artifactProbes)
	for _, v := range versions {
		g.Go(func() error {
			ok, err := check(gctx, v)
			if err != nil {
				return err
			}
			if ok {
				mu.Lock()
				found[v] = true
				mu.Unlock()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return found, nil
}
