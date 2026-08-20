// Package gcp implements the Cloud Run target.
//
// GCP has one store, and that shapes what the two reference schemes mean here.
// Cloud Run can mount a Secret Manager secret itself through secretKeyRef, and
// there is no separate configuration store it can mount — Parameter Manager
// exists but requires an explicit version in every resource name, with no
// "latest" alias, which makes it a poor fit for a reference written by hand.
//
// So on GCP both schemes read Secret Manager, and the difference between them
// is who resolves it: ${secret:…} is handed to Cloud Run untouched, ${param:…}
// is read here and written out as a literal. That is the same distinction as on
// the other clouds — it is about who does the reading — it just happens that
// the store is shared.
package gcp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
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

	services *run.ServicesClient
	secrets  *secretmanager.Client

	// registryClient is only needed by `update`, which asks which versions
	// exist, so it is built on first use rather than at startup.
	registryMu     sync.Mutex
	registryClient *artifactregistry.Client
}

// New builds a driver from application default credentials — in CI the token
// from the login-gcp action, locally a gcloud session.
func New(ctx context.Context, f *config.File) (*Driver, error) {
	services, err := run.NewServicesClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: %w", err)
	}
	secrets, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: %w", err)
	}
	return &Driver{file: f, services: services, secrets: secrets}, nil
}

func (d *Driver) Name() string { return "gcp" }

// Capabilities: Cloud Run resolves a Secret Manager reference itself. It has no
// equivalent for anything else, so ${param:…} is read here.
func (d *Driver) Capabilities(t config.TargetType) target.Capability {
	switch t {
	case config.TypeCloudRun:
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
		return d.planService(ctx, want)
	default:
		return nil, &target.ErrNotImplemented{Cloud: "gcp", Type: want.Target.Type}
	}
}

func (d *Driver) Apply(ctx context.Context, ch *target.Change) error {
	switch ch.Target.Type {
	case config.TypeCloudRun:
		return d.applyService(ctx, ch)
	default:
		return &target.ErrNotImplemented{Cloud: "gcp", Type: ch.Target.Type}
	}
}

func (d *Driver) Revert(ctx context.Context, ch *target.Change) error {
	switch ch.Target.Type {
	case config.TypeCloudRun:
		return d.revertService(ctx, ch)
	default:
		return &target.ErrNotImplemented{Cloud: "gcp", Type: ch.Target.Type}
	}
}

// serviceName builds the resource path Cloud Run expects.
func (d *Driver) serviceName(name string) string {
	return fmt.Sprintf("projects/%s/locations/%s/services/%s", d.file.Cloud.Project, d.file.Cloud.Region, name)
}

// --- references --------------------------------------------------------------

type resolver struct{ d *Driver }

// Verify confirms a secret exists without reading it. GetSecret returns
// metadata only and needs a lighter permission than accessing a version, so a
// reference that Cloud Run will resolve itself is checked without any value
// passing through this process.
func (r *resolver) Verify(ctx context.Context, v refs.Value) error {
	if !v.IsRef() {
		return nil
	}
	_, err := r.d.secrets.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{
		Name: r.secretPath(v.Name),
	})
	if err != nil {
		return fmt.Errorf("secret %q: %w", v.Name, err)
	}
	return nil
}

func (r *resolver) Read(ctx context.Context, v refs.Value) (string, error) {
	if !v.IsRef() {
		return v.Literal, nil
	}
	out, err := r.d.secrets.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: r.secretPath(v.Name) + "/versions/latest",
	})
	if err != nil {
		return "", fmt.Errorf("secret %q: %w", v.Name, err)
	}
	return string(out.GetPayload().GetData()), nil
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
