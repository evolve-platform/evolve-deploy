package gcp

import (
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

func TestVersionedSecretReferences(t *testing.T) {
	for _, tc := range []struct{ raw, name, version string }{
		{"token", "token", "latest"},
		{"token/versions/7", "token", "7"},
		{"projects/other/secrets/token/versions/8", "projects/other/secrets/token", "8"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			name, version, err := checkedSecretReference(tc.raw)
			if err != nil || name != tc.name || version != tc.version {
				t.Fatalf("got %q, %q, %v", name, version, err)
			}
			env := renderEnv([]target.EnvVar{{Name: "TOKEN", Value: refs.Value{Kind: refs.Secret, Name: tc.raw}}})
			ref := env[0].GetValueSource().GetSecretKeyRef()
			if ref.GetSecret() != tc.name || ref.GetVersion() != tc.version {
				t.Fatalf("reference = %v", ref)
			}
		})
	}
	for _, raw := range []string{"token/versions/", "token/versions/0", "token/versions/1/versions/2", "projects/p/secrets", "token/versions/current"} {
		if _, _, err := checkedSecretReference(raw); err == nil {
			t.Errorf("accepted invalid reference %q", raw)
		}
	}
}

func TestSecretVersionChangeTriggersDeployment(t *testing.T) {
	old := &runpb.Container{Env: renderEnv([]target.EnvVar{{Name: "TOKEN", Value: refs.Value{Kind: refs.Secret, Name: "token/versions/7"}}})}
	next := &runpb.Container{Env: renderEnv([]target.EnvVar{{Name: "TOKEN", Value: refs.Value{Kind: refs.Secret, Name: "token/versions/8"}}})}
	added, changed, removed := diffEnv([]*runpb.Container{old}, []*runpb.Container{next}, "", nil)
	if len(added) != 0 || len(removed) != 0 || len(changed) != 1 || changed[0] != "TOKEN" {
		t.Fatalf("diff = %v %v %v", added, changed, removed)
	}
	if old.Env[0].GetValueSource().GetSecretKeyRef().GetVersion() != "7" {
		t.Fatal("rollback reference was modified")
	}
}
