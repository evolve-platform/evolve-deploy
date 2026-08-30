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
	}, true, nil)
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

	next, _, err := nextTemplate(current, "", "abc1234", nil, false, nil)
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
	}, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	sidecar := findContainer(next.GetContainers(), "collector")
	if sidecar.GetImage() != "repo/otel:v9" {
		t.Errorf("the sidecar image was changed to %q", sidecar.GetImage())
	}
	if envFingerprint(sidecar.GetEnv(), nil)["OTEL_SERVICE_NAME"] != "=purchase" {
		t.Error("the sidecar environment was changed")
	}
}

// Terraform owns every field of the released container but the tag and the
// environment, which is why the next revision is built on the service's own
// template. A probe or a port an apply has just changed has to reach the next
// release rather than be staged back out: a liveness probe left polling the
// port the service no longer listens on killed every instance for failing it.
func TestTheReleasedContainerKeepsWhatTerraformOwns(t *testing.T) {
	current := template("r1", &runpb.Container{
		Name:  "app",
		Image: "repo/site:old",
		Ports: []*runpb.ContainerPort{{Name: "http1", ContainerPort: 4000}},
		LivenessProbe: &runpb.Probe{
			PeriodSeconds: 60,
			ProbeType: &runpb.Probe_HttpGet{
				HttpGet: &runpb.HTTPGetAction{Path: "/api/healthcheck", Port: 4000},
			},
		},
		Env: []*runpb.EnvVar{literal("STALE", "from-the-first-create")},
	})

	next, _, err := nextTemplate(current, "app", "2daef25", []target.EnvVar{
		{Name: "REDIS_URL", Value: refs.Value{Kind: refs.Literal, Literal: "redis://live"}},
	}, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	app := findContainer(next.GetContainers(), "app")
	if port := app.GetPorts()[0].GetContainerPort(); port != 4000 {
		t.Errorf("ContainerPort = %d, want the declared 4000", port)
	}
	probe := app.GetLivenessProbe()
	if probe.GetPeriodSeconds() != 60 || probe.GetHttpGet().GetPort() != 4000 {
		t.Errorf("liveness probe = %v, want the declared one", probe)
	}
	if got := envFingerprint(app.GetEnv(), nil); !reflect.DeepEqual(
		got, map[string]string{"REDIS_URL": "=redis://live"}) {
		t.Errorf("environment = %v, want the one the config declares", got)
	}
}

func TestReferencesBecomeSecretKeyRefs(t *testing.T) {
	got := renderEnv([]target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "info"}},
		{Name: "CTP_CLIENT_SECRET", Value: refs.Value{Kind: refs.Secret, Name: "purchase-ctp"}},
	})

	fp := envFingerprint(got, nil)
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

	added, changed, removed := diffEnv(current.GetContainers(), next.GetContainers(), "app", nil)
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

func TestADeclaredEnvironmentIsTheWholeEnvironment(t *testing.T) {
	// Cloud Run alone could have kept the merge -- the service is Terraform's to
	// correct, unlike an Azure container whose env is on ignore_changes. It does
	// not, because the same list of variables meaning the environment on one
	// cloud and a patch over an unseen one on another is not something a reader
	// of a deploy file can be asked to keep track of.
	current := template("purchase-00007-abc", &runpb.Container{
		Name:  "app",
		Image: "repo/purchase:old",
		Env: []*runpb.EnvVar{
			literal("LOG_LEVEL", "info"),
			literal("CTP_PROJECT_KEY", "evolve-tst"),
			secretRef("CTP_CLIENT_SECRET", "ctp-client-secret"),
		},
	})

	next, _, err := nextTemplate(current, "app", "abc1234", []target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "debug"}},
		{Name: "APP_CONFIG_ENDPOINT", Value: refs.Value{Kind: refs.Literal, Literal: "https://store"}},
	}, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	got := envFingerprint(findContainer(next.GetContainers(), "app").GetEnv(), nil)
	want := map[string]string{
		"LOG_LEVEL":           "=debug",
		"APP_CONFIG_ENDPOINT": "=https://store",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("environment = %v, want %v", got, want)
	}
}

func TestAConfigDeclaringNoEnvironmentKeepsWhatIsThere(t *testing.T) {
	// A repository that has not moved its environment here yet. Emptying one
	// would be a poor way to tell it so.
	current := template("purchase-00007-abc", &runpb.Container{
		Name:  "app",
		Image: "repo/purchase:old",
		Env: []*runpb.EnvVar{
			literal("LOG_LEVEL", "info"),
			secretRef("CTP_CLIENT_SECRET", "ctp-client-secret"),
		},
	})

	next, _, err := nextTemplate(current, "app", "abc1234", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}

	got := envFingerprint(findContainer(next.GetContainers(), "app").GetEnv(), nil)
	want := map[string]string{
		"LOG_LEVEL":         "=info",
		"CTP_CLIENT_SECRET": "->ctp-client-secret",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("environment = %v, want %v", got, want)
	}
}

func TestASidecarKeepsItsEnvironmentWhenTheConfigDeclaresOne(t *testing.T) {
	// Only the released container is rewritten. Replacing across the template
	// would empty the collector, whose environment is Terraform's.
	current := template("r1",
		&runpb.Container{
			Name:  "app",
			Image: "repo/purchase:old",
			Env:   []*runpb.EnvVar{literal("STALE", "yes")},
		},
		&runpb.Container{
			Name:  "collector",
			Image: "repo/otel:v9",
			Env:   []*runpb.EnvVar{literal("OTEL_SERVICE_NAME", "purchase")},
		},
	)

	next, _, err := nextTemplate(current, "app", "abc1234", []target.EnvVar{
		{Name: "APP_CONFIG_ENDPOINT", Value: refs.Value{Kind: refs.Literal, Literal: "https://store"}},
	}, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	sidecar := envFingerprint(findContainer(next.GetContainers(), "collector").GetEnv(), nil)
	if want := map[string]string{"OTEL_SERVICE_NAME": "=purchase"}; !reflect.DeepEqual(sidecar, want) {
		t.Errorf("sidecar environment = %v, want %v", sidecar, want)
	}

	app := envFingerprint(findContainer(next.GetContainers(), "app").GetEnv(), nil)
	if want := map[string]string{"APP_CONFIG_ENDPOINT": "=https://store"}; !reflect.DeepEqual(app, want) {
		t.Errorf("released container environment = %v, want %v", app, want)
	}
}
