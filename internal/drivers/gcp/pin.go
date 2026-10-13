package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/evolve-platform/evolve-deploy/internal/refs"
)

// Pin reads version metadata, never secret payloads. The returned ID is copied
// into the revision so an instance restart cannot follow a moving selector.
func (r *resolver) Pin(ctx context.Context, v refs.Value) (refs.Value, error) {
	switch v.Kind {
	case refs.ParameterVersion:
		return r.pinParameter(ctx, v)
	case refs.Secret:
		name, version, err := checkedSecretReference(v.Name)
		if err != nil {
			return refs.Value{}, err
		}
		got, err := r.d.secrets.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{
			Name: r.secretPath(name) + "/versions/" + version,
		})
		if err != nil {
			return refs.Value{}, fmt.Errorf("secret %q: %w", v.Name, err)
		}
		if got.GetState() != secretmanagerpb.SecretVersion_ENABLED {
			return refs.Value{}, fmt.Errorf("secret version %q is %s; use an enabled version", v.Name, got.GetState())
		}
		_, concrete, err := checkedSecretReference(got.GetName())
		if err != nil || concrete == "latest" {
			return refs.Value{}, fmt.Errorf("secret %q did not resolve to a numeric version", v.Name)
		}
		v.Name = name + "/versions/" + concrete
		return v, nil
	default:
		return v, r.Verify(ctx, v)
	}
}

var parameterReferencePattern = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/parameters/)?[A-Za-z0-9_][A-Za-z0-9_-]{0,62}$`)
var parameterVersionPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,62}$`)

func (r *resolver) pinParameter(ctx context.Context, v refs.Value) (refs.Value, error) {
	name, version := splitSecretVersion(v.Name)
	if !parameterReferencePattern.MatchString(name) || !parameterVersionPattern.MatchString(version) {
		return refs.Value{}, fmt.Errorf("invalid Parameter Manager reference %q: use NAME or projects/PROJECT/locations/LOCATION/parameters/NAME, optionally followed by /versions/VERSION", v.Name)
	}
	if !strings.HasPrefix(name, "projects/") {
		name = fmt.Sprintf("projects/%s/locations/global/parameters/%s", r.d.file.Cloud.Project, name)
	}
	got, err := r.d.parameters.Projects.Locations.Parameters.Versions.Get(name + "/versions/" + version).
		View("BASIC").Context(ctx).Do()
	if err != nil {
		return refs.Value{}, fmt.Errorf("parameter version %q: %w", v.Name, err)
	}
	if got.Disabled {
		return refs.Value{}, fmt.Errorf("parameter version %q is disabled; use an enabled version", v.Name)
	}
	parent, concrete, found := strings.Cut(got.Name, "/versions/")
	if !found || !strings.Contains(parent, "/parameters/") || concrete == "latest" || !parameterVersionPattern.MatchString(concrete) {
		return refs.Value{}, fmt.Errorf("parameter %q did not resolve to a concrete version", v.Name)
	}
	slog.Info("parameter version pinned", "parameter", name, "version", concrete)
	return refs.Value{Kind: refs.Literal, Literal: concrete, Raw: v.Raw}, nil
}
