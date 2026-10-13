// Package gcp implements the Cloud Run targets.
//
// Both reference schemes use Secret Manager: secret references are mounted by
// Cloud Run, while parameter references are read during planning. An explicit
// /versions/N suffix keeps either reference tied to the release that chose it.
package gcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	run "cloud.google.com/go/run/apiv2"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// Driver is the GCP implementation of target.Driver.
type Driver struct {
	file *config.File

	services  *run.ServicesClient
	revisions *run.RevisionsClient
	jobs      *run.JobsClient
	secrets   *secretmanager.Client

	// executions only cancels: a job hook's run that outlived its timeout.
	executions *run.ExecutionsClient
}

// New builds a driver from application default credentials — in CI the token
// from the login-gcp action, locally a gcloud session.
func New(ctx context.Context, f *config.File) (*Driver, error) {
	services, err := run.NewServicesClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: %w", err)
	}
	// Read-only, and only on the blue-green path: with two live revisions the
	// service's own template is whichever was created last, so the version that
	// is serving has to be read off the revision that is serving.
	revisions, err := run.NewRevisionsClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: %w", err)
	}
	jobs, err := run.NewJobsClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: %w", err)
	}
	secrets, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: %w", err)
	}
	executions, err := run.NewExecutionsClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: %w", err)
	}
	return &Driver{
		file:      f,
		services:  services,
		revisions: revisions,
		jobs:      jobs,
		secrets:   secrets,

		executions: executions,
	}, nil
}

func (d *Driver) Name() string { return "gcp" }

// Capabilities: Cloud Run resolves a Secret Manager reference itself, in a job
// exactly as in a service — the container is the same message either side. It
// has no equivalent for anything else, so ${param:…} is read here.
func (d *Driver) Capabilities(t config.TargetType) target.Capability {
	switch t {
	case config.TypeCloudRun, config.TypeCloudRunJob:
		return target.Capability{NativeSecret: true}
	default:
		return target.Capability{}
	}
}

// Verify has nothing to guard: the project and region are parameters on every
// call, so naming the wrong one produces a "not found" rather than a deploy to
// the wrong place. Compare AWS, where the account is implicit in the
// credentials and therefore has to be checked.
func (d *Driver) Verify(_ context.Context) error { return nil }

func (d *Driver) Resolver() refs.Resolver { return &resolver{d: d} }

func (d *Driver) Plan(ctx context.Context, want *target.Desired) (*target.Change, error) {
	switch want.Target.Type {
	case config.TypeCloudRun:
		if want.Target.Strategy.IsBlueGreen() {
			return d.planServiceBlueGreen(ctx, want)
		}
		return d.planService(ctx, want)
	case config.TypeCloudRunJob:
		return d.planJob(ctx, want)
	default:
		return nil, &target.ErrNotImplemented{Cloud: "gcp", Type: want.Target.Type}
	}
}

func (d *Driver) Apply(ctx context.Context, ch *target.Change) error {
	switch ch.Target.Type {
	case config.TypeCloudRun:
		return d.applyService(ctx, ch)
	case config.TypeCloudRunJob:
		return d.applyJob(ctx, ch)
	default:
		return &target.ErrNotImplemented{Cloud: "gcp", Type: ch.Target.Type}
	}
}

func (d *Driver) Revert(ctx context.Context, ch *target.Change) error {
	switch ch.Target.Type {
	case config.TypeCloudRun:
		return d.revertService(ctx, ch)
	case config.TypeCloudRunJob:
		return d.revertJob(ctx, ch)
	default:
		return &target.ErrNotImplemented{Cloud: "gcp", Type: ch.Target.Type}
	}
}

// serviceName builds the resource path Cloud Run expects.
func (d *Driver) serviceName(name string) string {
	return fmt.Sprintf("projects/%s/locations/%s/services/%s", d.file.Cloud.Project, d.file.Cloud.Region, name)
}

// jobName is serviceName for the other collection.
func (d *Driver) jobName(name string) string {
	return fmt.Sprintf("projects/%s/locations/%s/jobs/%s", d.file.Cloud.Project, d.file.Cloud.Region, name)
}

// --- references --------------------------------------------------------------

type resolver struct{ d *Driver }

// Verify checks metadata without accessing secret values. Versioned references
// must remain enabled so an old revision can still cold-start during rollback.
func (r *resolver) Verify(ctx context.Context, v refs.Value) error {
	if !v.IsRef() {
		return nil
	}
	name, version, err := checkedSecretReference(v.Name)
	if err != nil {
		return err
	}
	if version == "latest" {
		_, err = r.d.secrets.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: r.secretPath(name)})
		return err
	}
	got, err := r.d.secrets.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{
		Name: r.secretPath(name) + "/versions/" + version,
	})
	if err != nil {
		return fmt.Errorf("secret %q: %w", v.Name, err)
	}
	if got.GetState() != secretmanagerpb.SecretVersion_ENABLED {
		return fmt.Errorf("secret version %q is %s; use an enabled version", v.Name, got.GetState())
	}
	return nil
}

func (r *resolver) Read(ctx context.Context, v refs.Value) (string, error) {
	if !v.IsRef() {
		return v.Literal, nil
	}
	name, version, err := checkedSecretReference(v.Name)
	if err != nil {
		return "", err
	}
	out, err := r.d.secrets.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: r.secretPath(name) + "/versions/" + version,
	})
	if err != nil {
		return "", fmt.Errorf("secret %q: %w", v.Name, err)
	}
	return string(out.GetPayload().GetData()), nil
}

var secretReferencePattern = regexp.MustCompile(`^(projects/[^/]+/secrets/)?[A-Za-z0-9_-]+$`)
var secretVersionPattern = regexp.MustCompile(`^(latest|[1-9][0-9]*)$`)

func splitSecretVersion(raw string) (name, version string) {
	name, version, found := strings.Cut(raw, "/versions/")
	if !found {
		version = "latest"
	}
	return name, version
}

func checkedSecretReference(raw string) (name, version string, err error) {
	name, version = splitSecretVersion(raw)
	if !secretReferencePattern.MatchString(name) || !secretVersionPattern.MatchString(version) {
		return "", "", fmt.Errorf("invalid Secret Manager reference %q: use a secret name or projects/PROJECT/secrets/NAME, optionally followed by /versions/NUMBER", raw)
	}
	return name, version, nil
}

func (r *resolver) ReadMap(ctx context.Context, v refs.Value) (map[string]string, error) {
	raw, err := r.Read(ctx, v)
	if err != nil {
		return nil, err
	}
	return parseJSONMap(v.Raw, raw)
}

// secretPath accepts either a bare secret name or a full resource path, so a
// secret in another project can still be named.
func (r *resolver) secretPath(name string) string {
	if strings.HasPrefix(name, "projects/") {
		return name
	}
	return fmt.Sprintf("projects/%s/secrets/%s", r.d.file.Cloud.Project, name)
}
