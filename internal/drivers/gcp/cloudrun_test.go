package gcp

import (
	"reflect"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

func literal(name, value string) *runpb.EnvVar {
	return &runpb.EnvVar{Name: name, Values: &runpb.EnvVar_Value{Value: value}}
}

func secretRef(name, secret string) *runpb.EnvVar {
	return &runpb.EnvVar{
		Name: name,
		Values: &runpb.EnvVar_ValueSource{
			ValueSource: &runpb.EnvVarSource{
				SecretKeyRef: &runpb.SecretKeySelector{Secret: secret, Version: "latest"},
			},
		},
	}
}

func template(revision string, containers ...*runpb.Container) *runpb.RevisionTemplate {
	return &runpb.RevisionTemplate{Revision: revision, Containers: containers}
}

func TestTheUnnamedContainerIsFound(t *testing.T) {
	// The cloud-run-service module leaves its single container unnamed, because
	// Cloud Run only requires names once there are sidecars.
	current := template("purchase-00007-abc", &runpb.Container{
		Image: "europe-west4-docker.pkg.dev/mgmt/evolve/purchase:old",
		Env:   []*runpb.EnvVar{literal("LOG_LEVEL", "info")},
	})

	name, err := target.PickContainer(
		containerNames(current.GetContainers()), "", cloudRunContainer)
	if err != nil {
		t.Fatal(err)
	}

	next, from, err := nextTemplate(current, name, "abc1234", []target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "debug"}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if from != "old" {
		t.Errorf("from = %q", from)
	}
	if got := next.GetContainers()[0].GetImage(); got != "europe-west4-docker.pkg.dev/mgmt/evolve/purchase:abc1234" {
		t.Errorf("image = %q", got)
	}
}

func TestRevisionNameIsCleared(t *testing.T) {
	// A revision name is unique per revision, so carrying the old one over
	// makes Cloud Run reject the update.
	current := template("purchase-00007-abc", &runpb.Container{Image: "repo/purchase:old"})

	next, _, err := nextTemplate(current, "", "abc1234", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if next.GetRevision() != "" {
		t.Errorf("Revision = %q, want it cleared", next.GetRevision())
	}
	// And the original must not have been mutated — it is what a rollback
	// restores.
	if current.GetRevision() != "purchase-00007-abc" {
		t.Error("the current template was modified in place")
	}
	if current.GetContainers()[0].GetImage() != "repo/purchase:old" {
		t.Error("the current template's image was modified in place")
	}
}

func TestSidecarsAreLeftAlone(t *testing.T) {
	current := template("r1",
		&runpb.Container{
			Name:  "app",
			Image: "repo/purchase:old",
			Env:   []*runpb.EnvVar{literal("LOG_LEVEL", "info")},
		},
		&runpb.Container{
			Name:  "collector",
			Image: "repo/otel:v9",
			Env:   []*runpb.EnvVar{literal("OTEL_SERVICE_NAME", "purchase")},
		},
	)

	next, _, err := nextTemplate(current, "app", "abc1234", []target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "debug"}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	sidecar := findContainer(next.GetContainers(), "collector")
	if sidecar.GetImage() != "repo/otel:v9" {
		t.Errorf("the sidecar image was changed to %q", sidecar.GetImage())
	}
	if envFingerprint(sidecar.GetEnv())["OTEL_SERVICE_NAME"] != "=purchase" {
		t.Error("the sidecar environment was changed")
	}
}

func TestReferencesBecomeSecretKeyRefs(t *testing.T) {
	got := renderEnv([]target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "info"}},
		{Name: "CTP_CLIENT_SECRET", Value: refs.Value{Kind: refs.Secret, Name: "purchase-ctp"}},
	})

	fp := envFingerprint(got)
	if fp["LOG_LEVEL"] != "=info" {
		t.Errorf("LOG_LEVEL = %q", fp["LOG_LEVEL"])
	}
	if fp["CTP_CLIENT_SECRET"] != "->purchase-ctp" {
		t.Errorf("CTP_CLIENT_SECRET = %q, want a secretKeyRef", fp["CTP_CLIENT_SECRET"])
	}
	// Cloud Run resolves the reference when the revision starts, so the value
	// never passes through this tool.
	for _, e := range got {
		if e.GetName() == "CTP_CLIENT_SECRET" && e.GetValue() != "" {
			t.Error("a secret reference was written out as a literal value")
		}
	}
}

func TestDiffEnvIgnoresRotations(t *testing.T) {
	current := template("r1", &runpb.Container{
		Name:  "app",
		Image: "repo/purchase:old",
		Env: []*runpb.EnvVar{
			literal("LOG_LEVEL", "info"),
			literal("GONE", "x"),
			secretRef("CTP_CLIENT_SECRET", "purchase-ctp"),
		},
	})
	next := template("", &runpb.Container{
		Name:  "app",
		Image: "repo/purchase:new",
		Env: []*runpb.EnvVar{
			literal("LOG_LEVEL", "debug"),
			literal("NEW", "y"),
			secretRef("CTP_CLIENT_SECRET", "purchase-ctp"),
		},
	})

	added, changed, removed := diffEnv(current, next, "app")
	if !reflect.DeepEqual(added, []string{"NEW"}) {
		t.Errorf("added = %v", added)
	}
	if !reflect.DeepEqual(changed, []string{"LOG_LEVEL"}) {
		t.Errorf("changed = %v", changed)
	}
	if !reflect.DeepEqual(removed, []string{"GONE"}) {
		t.Errorf("removed = %v", removed)
	}
	// A reference still pointing at the same secret has not changed, whatever
	// that secret now holds.
	for _, name := range changed {
		if name == "CTP_CLIENT_SECRET" {
			t.Error("an unchanged reference was reported as changed")
		}
	}
}
