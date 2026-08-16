package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/evolve-platform/evolve-deploy/internal/image"
)

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
