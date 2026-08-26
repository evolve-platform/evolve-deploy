package gcp

import (
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// job builds what GetJob returns: the containers sit two messages down, and
// everything around them is Terraform's.
func job(etag string, containers ...*runpb.Container) *runpb.Job {
	return &runpb.Job{
		Name: "projects/evolve-tst/locations/europe-west4/jobs/discover-products",
		Etag: etag,
		Labels: map[string]string{
			"managed-by": "terraform",
		},
		Template: &runpb.ExecutionTemplate{
			Parallelism: 4,
			TaskCount:   4,
			Template: &runpb.TaskTemplate{
				Containers:     containers,
				ServiceAccount: "discover@evolve-tst.iam.gserviceaccount.com",
				Timeout:        durationpb.New(time.Hour),
				Retries:        &runpb.TaskTemplate_MaxRetries{MaxRetries: 2},
			},
		},
	}
}

func TestAJobKeepsEverythingTerraformSetOnIt(t *testing.T) {
	// UpdateJob takes no field mask, so the whole resource is written back and
	// everything not touched here has to survive the round trip.
	current := job("v1", &runpb.Container{
		Image: "europe-west4-docker.pkg.dev/mgmt/evolve/discover:old",
		Args:  []string{"evolve", "sync", "products"},
		Env:   []*runpb.EnvVar{literal("LOG_LEVEL", "info")},
	})

	next, from, err := nextJob(current, "", "abc1234", []target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "debug"}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if from != "old" {
		t.Errorf("from = %q", from)
	}

	c := jobContainers(next)[0]
	if got := c.GetImage(); got != "europe-west4-docker.pkg.dev/mgmt/evolve/discover:abc1234" {
		t.Errorf("image = %q", got)
	}
	// The arguments are what makes one job different from the next, and they
	// are Terraform's: four sync jobs share one image and differ only here.
	if got := c.GetArgs(); len(got) != 3 || got[2] != "products" {
		t.Errorf("args = %v", got)
	}
	if fp := envFingerprint(c.GetEnv(), nil); fp["LOG_LEVEL"] != "=debug" {
		t.Errorf("LOG_LEVEL = %q", fp["LOG_LEVEL"])
	}

	tmpl := next.GetTemplate()
	if tmpl.GetParallelism() != 4 || tmpl.GetTaskCount() != 4 {
		t.Errorf("parallelism = %d, task count = %d", tmpl.GetParallelism(), tmpl.GetTaskCount())
	}
	task := tmpl.GetTemplate()
	if task.GetServiceAccount() != "discover@evolve-tst.iam.gserviceaccount.com" {
		t.Errorf("service account = %q", task.GetServiceAccount())
	}
	if task.GetMaxRetries() != 2 || task.GetTimeout().GetSeconds() != 3600 {
		t.Errorf("retries = %d, timeout = %v", task.GetMaxRetries(), task.GetTimeout())
	}
	if next.GetLabels()["managed-by"] != "terraform" {
		t.Errorf("labels = %v", next.GetLabels())
	}
	// The etag rides along, so a job Terraform moved between the plan and the
	// apply fails the write instead of silently overwriting it.
	if next.GetEtag() != "v1" {
		t.Errorf("etag = %q", next.GetEtag())
	}

	if current.GetTemplate().GetTemplate().GetContainers()[0].GetImage() !=
		"europe-west4-docker.pkg.dev/mgmt/evolve/discover:old" {
		t.Error("the job that was read was modified in place")
	}
}

func TestAJobSidecarIsLeftAlone(t *testing.T) {
	current := job("",
		&runpb.Container{
			Name:  "app",
			Image: "repo/discover:old",
			Env:   []*runpb.EnvVar{literal("LOG_LEVEL", "info")},
		},
		&runpb.Container{
			Name:  "collector",
			Image: "repo/otel:v9",
			Env:   []*runpb.EnvVar{literal("OTEL_SERVICE_NAME", "discover")},
		},
	)

	next, _, err := nextJob(current, "app", "abc1234", []target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "debug"}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	sidecar := findContainer(jobContainers(next), "collector")
	if sidecar.GetImage() != "repo/otel:v9" {
		t.Errorf("the sidecar image was changed to %q", sidecar.GetImage())
	}
	if envFingerprint(sidecar.GetEnv(), nil)["OTEL_SERVICE_NAME"] != "=discover" {
		t.Error("the sidecar environment was changed")
	}
}

func TestAJobConfigDeclaringNoEnvironmentKeepsWhatIsThere(t *testing.T) {
	current := job("", &runpb.Container{
		Image: "repo/discover:old",
		Env: []*runpb.EnvVar{
			literal("LOG_LEVEL", "info"),
			secretRef("CTP_CLIENT_SECRET", "discover-ctp"),
		},
	})

	next, _, err := nextJob(current, "", "abc1234", nil, false)
	if err != nil {
		t.Fatal(err)
	}

	fp := envFingerprint(jobContainers(next)[0].GetEnv(), nil)
	if fp["LOG_LEVEL"] != "=info" || fp["CTP_CLIENT_SECRET"] != "->discover-ctp" {
		t.Errorf("the environment was rewritten: %v", fp)
	}

	added, changed, removed := diffEnv(jobContainers(current), jobContainers(next), "", nil)
	if len(added)+len(changed)+len(removed) != 0 {
		t.Errorf("image-only deploy reported +%v ~%v -%v", added, changed, removed)
	}
}

func TestAJobReferenceBecomesASecretKeyRef(t *testing.T) {
	current := job("", &runpb.Container{Image: "repo/discover:old"})

	next, _, err := nextJob(current, "", "abc1234", []target.EnvVar{
		{Name: "CTP_CLIENT_SECRET", Value: refs.Value{Kind: refs.Secret, Name: "discover-ctp"}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	src := jobContainers(next)[0].GetEnv()[0].GetValueSource().GetSecretKeyRef()
	if src.GetSecret() != "discover-ctp" || src.GetVersion() != secretVersion {
		t.Errorf("secretKeyRef = %v", src)
	}
}

func TestARollbackWriteDropsTheEtag(t *testing.T) {
	// The state has moved on since the plan read it — that is why there is
	// something to undo — so a rollback must not be refused over it.
	previous := job("v1", &runpb.Container{Image: "repo/discover:old"})

	if got := stale(previous).GetEtag(); got != "" {
		t.Errorf("etag = %q, want it dropped", got)
	}
	if previous.GetEtag() != "v1" {
		t.Error("the job held for the rollback was modified in place")
	}
}

func TestAJobWithSeveralContainersAndNoNameIsRefused(t *testing.T) {
	current := job("",
		&runpb.Container{Name: "app", Image: "repo/discover:old"},
		&runpb.Container{Name: "collector", Image: "repo/otel:v9"},
	)

	_, err := target.PickContainer(
		containerNames(jobContainers(current)), "", cloudRunContainer)
	if err == nil {
		t.Fatal("want a refusal naming what is there")
	}
}
