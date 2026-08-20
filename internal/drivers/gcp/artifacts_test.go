package gcp

import (
	"errors"
	"testing"

	"github.com/evolve-platform/evolve-deploy/internal/target"
)

func TestPackageName(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{
			name: "plain image",
			ref:  "europe-west4-docker.pkg.dev/evolve-tst/services/discover:abc1234",
			want: "projects/evolve-tst/locations/europe-west4/repositories/services/packages/discover",
		},
		{
			// A package id holds the whole path below the repository, and the
			// API only accepts it encoded.
			name: "nested image",
			ref:  "europe-west4-docker.pkg.dev/evolve-tst/services/team/discover:abc1234",
			want: "projects/evolve-tst/locations/europe-west4/repositories/services/packages/team%2Fdiscover",
		},
		{
			name: "multi-region host",
			ref:  "us-docker.pkg.dev/evolve-tst/services/discover:abc1234",
			want: "projects/evolve-tst/locations/us/repositories/services/packages/discover",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := packageName(c.ref)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("packageName(%q) =\n  %s\nwant\n  %s", c.ref, got, c.want)
			}
		})
	}
}

// An image outside Artifact Registry is deployable but not listable, and that
// difference has to reach the caller as ErrArtifactsUnknown — anything else
// would turn "I cannot check" into "nothing was built".
func TestPackageNameOutsideArtifactRegistry(t *testing.T) {
	for _, ref := range []string{
		"eu.gcr.io/evolve-tst/discover:abc1234",
		"docker.io/library/nginx:1.27",
		"nginx:1.27",
	} {
		_, err := packageName(ref)
		if !errors.Is(err, target.ErrArtifactsUnknown) {
			t.Errorf("packageName(%q) error = %v, want ErrArtifactsUnknown", ref, err)
		}
	}
}
