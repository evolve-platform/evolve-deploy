package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	parametermanager "google.golang.org/api/parametermanager/v1"

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

var parameterReferencePattern = regexp.MustCompile(`^(projects/[^/]+/locations/global/parameters/)?[A-Za-z0-9_][A-Za-z0-9_-]{0,62}$`)
var parameterVersionPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,62}$`)

func (r *resolver) pinParameter(ctx context.Context, v refs.Value) (refs.Value, error) {
	name, version := splitSecretVersion(v.Name)
	if !parameterReferencePattern.MatchString(name) || !parameterVersionPattern.MatchString(version) {
		return refs.Value{}, fmt.Errorf("invalid Parameter Manager reference %q: use NAME or projects/PROJECT/locations/global/parameters/NAME, optionally followed by /versions/VERSION; regional parameters are not supported", v.Name)
	}
	if !strings.HasPrefix(name, "projects/") {
		name = fmt.Sprintf("projects/%s/locations/global/parameters/%s", r.d.file.Cloud.Project, name)
	}
	var got *parametermanager.ParameterVersion
	var err error
	if version == "latest" {
		got, err = r.latestParameterVersion(ctx, name)
	} else {
		got, err = r.d.parameters.Projects.Locations.Parameters.Versions.Get(name + "/versions/" + version).
			View("BASIC").Context(ctx).Do()
	}
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

// The render API resolves latest, but the metadata Get API requires a real ID.
// Listing metadata avoids rendering secrets just to discover that ID. Compare
// creation times across every page rather than relying on unspecified ordering.
func (r *resolver) latestParameterVersion(ctx context.Context, name string) (*parametermanager.ParameterVersion, error) {
	var latest *parametermanager.ParameterVersion
	var newest time.Time
	var ambiguous bool
	err := r.d.parameters.Projects.Locations.Parameters.Versions.List(name).
		Fields("parameterVersions(name,createTime,disabled),nextPageToken,unreachable").
		Pages(ctx, func(page *parametermanager.ListParameterVersionsResponse) error {
			if len(page.Unreachable) > 0 {
				return fmt.Errorf("cannot determine the latest version; locations unreachable: %s", strings.Join(page.Unreachable, ", "))
			}
			for _, v := range page.ParameterVersions {
				if v == nil {
					return fmt.Errorf("received empty parameter version metadata")
				}
				created, err := time.Parse(time.RFC3339Nano, v.CreateTime)
				if err != nil {
					return fmt.Errorf("parameter version %q has an invalid creation time: %w", v.Name, err)
				}
				if latest == nil || created.After(newest) {
					latest, newest, ambiguous = v, created, false
				} else if created.Equal(newest) && v.Name != latest.Name {
					ambiguous = true
				}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, fmt.Errorf("no parameter versions exist; publish a version before deploying")
	}
	if ambiguous {
		return nil, fmt.Errorf("newest parameter versions share a creation time; select an explicit /versions/VERSION")
	}
	return latest, nil
}
