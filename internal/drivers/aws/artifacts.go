package aws

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/sync/errgroup"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
	"github.com/evolve-platform/evolve-deploy/internal/tmpl"
)

// batchGetImageLimit is the most image ids ECR accepts in one BatchGetImage.
const batchGetImageLimit = 100

// artifactProbes bounds how many objects are checked at once. The candidates
// for one target are checked in parallel because they are independent; the
// limit keeps a long version list from arriving at S3 as a burst.
const artifactProbes = 8

// verifyImage checks that the tag exists before anything is deployed.
//
// The pipeline lists a service as changed and hands its git sha to the deploy
// even when its build job failed, so without this check the rollout starts and
// then stalls on an image that was never pushed. Finding it in the plan phase
// costs one API call and keeps the service up.
func (d *Driver) verifyImage(ctx context.Context, ref string) error {
	registryID, repo, tag, ok := parseECRImage(ref)
	if !ok {
		// Not an ECR image. Verifying an arbitrary registry would mean handling
		// its auth, which is not worth it — the rollout will report a pull
		// failure soon enough.
		return nil
	}

	_, err := d.ecr.DescribeImages(ctx, &ecr.DescribeImagesInput{
		RegistryId:     awssdk.String(registryID),
		RepositoryName: awssdk.String(repo),
		ImageIds:       []ecrtypes.ImageIdentifier{{ImageTag: awssdk.String(tag)}},
	})
	if err == nil {
		return nil
	}

	var notFound *ecrtypes.ImageNotFoundException
	if errors.As(err, &notFound) {
		return fmt.Errorf("image %s does not exist (did its build job fail?)", ref)
	}
	var noRepo *ecrtypes.RepositoryNotFoundException
	if errors.As(err, &noRepo) {
		return fmt.Errorf("ECR repository %s does not exist in account %s", repo, registryID)
	}
	return fmt.Errorf("checking image %s: %w", ref, err)
}

// verifyObject is the same check for a Lambda deployment package.
func (d *Driver) verifyObject(ctx context.Context, bucket, key string) error {
	_, err := d.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: awssdk.String(bucket),
		Key:    awssdk.String(key),
	})
	if err == nil {
		return nil
	}
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return fmt.Errorf("s3://%s/%s does not exist (did its build job fail?)", bucket, key)
	}
	return fmt.Errorf("checking s3://%s/%s: %w", bucket, key, err)
}

// parseECRImage splits an ECR image reference. It reports false for anything
// that is not an ECR registry host, including public images.
func parseECRImage(ref string) (registryID, repo, tag string, ok bool) {
	repoPath, tag := image.Split(ref)
	if tag == "" {
		return "", "", "", false
	}
	host, path, found := strings.Cut(repoPath, "/")
	if !found || !strings.Contains(host, ".dkr.ecr.") {
		return "", "", "", false
	}
	registryID, _, _ = strings.Cut(host, ".")
	return registryID, path, tag, true
}

// Artifacts reports which of the given versions could be deployed to this
// target.
func (d *Driver) Artifacts(
	ctx context.Context, t *config.Target, versions []string,
) ([]string, error) {
	switch t.Type {
	case config.TypeECS:
		return d.imageVersions(ctx, t, versions)
	case config.TypeLambda:
		return d.packageVersions(ctx, t, versions)
	default:
		return nil, &target.ErrNotImplemented{Cloud: d.Name(), Type: t.Type}
	}
}

// imageVersions asks ECR which of the tags exist.
//
// The repository comes from the base task definition, because that is the image
// planECS retags — asking the running service instead would look at the same
// repository via a revision that is about to be replaced.
func (d *Driver) imageVersions(
	ctx context.Context, t *config.Target, versions []string,
) ([]string, error) {
	base, err := d.describeTaskDef(ctx, t.Base)
	if err != nil {
		return nil, fmt.Errorf("base task definition %q: %w", t.Base, err)
	}
	name, err := target.PickContainer(
		containerNames(base.ContainerDefinitions), t.Container, ecsAppContainer)
	if err != nil {
		return nil, fmt.Errorf("base task definition %s: %w", t.Base, err)
	}
	ref := awssdk.ToString(findContainer(base.ContainerDefinitions, name).Image)

	registryID, repo, _, ok := parseECRImage(ref)
	if !ok {
		return nil, fmt.Errorf("%s: %w", ref, target.ErrArtifactsUnknown)
	}

	have := map[string]bool{}
	// BatchGetImage rather than DescribeImages: it reports what is missing in a
	// failures list instead of erroring, which is the whole point here — most of
	// the tags asked about are expected not to exist. It also needs no
	// permission beyond the ecr:BatchGetImage that pulling an image already
	// requires.
	for chunk := range slices.Chunk(versions, batchGetImageLimit) {
		ids := make([]ecrtypes.ImageIdentifier, len(chunk))
		for i, v := range chunk {
			ids[i] = ecrtypes.ImageIdentifier{ImageTag: awssdk.String(v)}
		}

		timer := logging.Start("batch get image", "repository", repo, "tags", len(ids))
		out, err := d.ecr.BatchGetImage(ctx, &ecr.BatchGetImageInput{
			RegistryId:     awssdk.String(registryID),
			RepositoryName: awssdk.String(repo),
			ImageIds:       ids,
		})
		if err != nil {
			var noRepo *ecrtypes.RepositoryNotFoundException
			if errors.As(err, &noRepo) {
				return nil, fmt.Errorf("ECR repository %s does not exist in account %s",
					repo, registryID)
			}
			return nil, fmt.Errorf("listing images in %s: %w", repo, err)
		}
		timer.Done("found", len(out.Images))

		for _, img := range out.Images {
			if img.ImageId != nil {
				have[awssdk.ToString(img.ImageId.ImageTag)] = true
			}
		}
	}
	return target.Present(versions, have), nil
}

// packageVersions checks which zips exist, one HEAD per candidate.
//
// Listing the bucket would be one call instead of thirty, but it needs
// s3:ListBucket, which a deploy role that only ever fetches a known key does
// not have. A miss is a 404 and costs nothing, so the cheap permission is worth
// the extra calls.
func (d *Driver) packageVersions(
	ctx context.Context, t *config.Target, versions []string,
) ([]string, error) {
	if t.Code == nil {
		return nil, fmt.Errorf("no code.bucket/code.key for %s", t.Label())
	}

	found := make([]bool, len(versions))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(artifactProbes)

	for i, v := range versions {
		g.Go(func() error {
			key, err := tmpl.Render(t.Code.Key, map[string]string{
				"version": v,
				"name":    t.Name,
			})
			if err != nil {
				return fmt.Errorf("code.key: %w", err)
			}
			_, err = d.s3.HeadObject(gctx, &s3.HeadObjectInput{
				Bucket: awssdk.String(t.Code.Bucket),
				Key:    awssdk.String(key),
			})
			if err == nil {
				found[i] = true
				return nil
			}

			var notFound *s3types.NotFound
			if errors.As(err, &notFound) {
				return nil
			}
			// A 403 on a HEAD is what a bucket policy that allows GetObject on
			// a prefix but nothing else returns, and it says nothing about
			// whether the object is there. Reporting it as absent would quietly
			// shorten the list, so it fails loudly instead.
			return fmt.Errorf("checking s3://%s/%s: %w", t.Code.Bucket, key, err)
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	have := map[string]bool{}
	for i, ok := range found {
		if ok {
			have[versions[i]] = true
		}
	}
	return target.Present(versions, have), nil
}
