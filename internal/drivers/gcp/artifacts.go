package gcp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/run/apiv2/runpb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// artifactProbes bounds how many tags are looked up at once, so a long version
// list does not arrive at Artifact Registry as a burst.
const artifactProbes = 8

// Artifacts reports which of the given versions could be deployed to this
// target.
func (d *Driver) Artifacts(
	ctx context.Context, t *config.Target, versions []string,
) ([]string, error) {
	if t.Type != config.TypeCloudRun {
		return nil, &target.ErrNotImplemented{Cloud: d.Name(), Type: t.Type}
	}

	// The registry and repository are never in the config — only the tag is
	// replaced — so the running service is where they come from.
	svc, err := d.services.GetService(ctx, &runpb.GetServiceRequest{
		Name: d.serviceName(t.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("cloud run service %s: %w", t.Name, err)
	}
	if svc.GetTemplate() == nil {
		return nil, fmt.Errorf("cloud run service %s has no template", t.Name)
	}
	name, err := target.PickContainer(
		containerNames(svc.GetTemplate().GetContainers()), t.Container, cloudRunContainer)
	if err != nil {
		return nil, fmt.Errorf("cloud run service %s: %w", t.Name, err)
	}
	container := findContainer(svc.GetTemplate().GetContainers(), name)
	if container == nil {
		return nil, fmt.Errorf("cloud run service %s: container %q disappeared", t.Name, name)
	}

	pkg, err := packageName(container.GetImage())
	if err != nil {
		return nil, err
	}

	client, err := d.registry(ctx)
	if err != nil {
		return nil, err
	}

	// One GetTag per candidate rather than listing the package's tags: a
	// repository that has been building for a year holds thousands of them, in
	// no useful order, and a capped listing would report a version as missing
	// only because it was too far down the list. A miss here is a NotFound and
	// costs nothing.
	timer := logging.Start("get tags", "package", pkg, "tags", len(versions))
	var (
		mu    sync.Mutex
		have  = map[string]bool{}
		g, gc = errgroup.WithContext(ctx)
	)
	g.SetLimit(artifactProbes)

	for _, v := range versions {
		g.Go(func() error {
			_, err := client.GetTag(gc, &artifactregistrypb.GetTagRequest{
				Name: pkg + "/tags/" + v,
			})
			switch {
			case err == nil:
				mu.Lock()
				have[v] = true
				mu.Unlock()
				return nil
			case status.Code(err) == codes.NotFound:
				// Expected for most candidates: a commit that did not touch
				// this service was never built for it.
				return nil
			default:
				return fmt.Errorf("reading tag %s of %s: %w", v, pkg, err)
			}
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	timer.Done("found", len(have), "of", len(versions))

	return target.Present(versions, have), nil
}

// registry builds the Artifact Registry client on first use.
//
// A deploy never reads a tag list, so a repository that only ever runs apply
// pays nothing for this — and one running in an environment where the API is
// not enabled does not fail at startup for a client it will not use.
func (d *Driver) registry(ctx context.Context) (*artifactregistry.Client, error) {
	d.registryMu.Lock()
	defer d.registryMu.Unlock()

	if d.registryClient != nil {
		return d.registryClient, nil
	}
	client, err := artifactregistry.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("artifact registry: %w", err)
	}
	d.registryClient = client
	return client, nil
}

// packageName turns an image reference into the resource a tag lives under.
//
// europe-west4-docker.pkg.dev/evolve/services/discover becomes
// projects/evolve/locations/europe-west4/repositories/services/packages/discover,
// and an image nested deeper than that keeps its slashes escaped — a package id
// holds the whole path, which the API only accepts encoded.
func packageName(ref string) (string, error) {
	repo, _ := image.Split(ref)

	parts := strings.Split(repo, "/")
	if len(parts) < 4 || !strings.HasSuffix(parts[0], "-docker.pkg.dev") {
		// gcr.io, Docker Hub, anything else: still deployable, just not
		// listable from here.
		return "", fmt.Errorf("%s: %w", ref, target.ErrArtifactsUnknown)
	}

	location := strings.TrimSuffix(parts[0], "-docker.pkg.dev")
	project, repository, pkg := parts[1], parts[2], strings.Join(parts[3:], "/")

	return fmt.Sprintf("projects/%s/locations/%s/repositories/%s/packages/%s",
		project, location, repository, url.PathEscape(pkg)), nil
}
