package aws

import (
	"reflect"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

func TestParseECRImage(t *testing.T) {
	registry, repo, tag, ok := parseECRImage("513712104672.dkr.ecr.eu-west-1.amazonaws.com/purchase:abc")
	if !ok {
		t.Fatal("expected an ECR image")
	}
	if registry != "513712104672" || repo != "purchase" || tag != "abc" {
		t.Errorf("got (%q, %q, %q)", registry, repo, tag)
	}

	// Anything that is not ECR is skipped rather than guessed at, because
	// verifying it would mean handling that registry's auth.
	if _, _, _, ok := parseECRImage("europe-west4-docker.pkg.dev/x/purchase:abc"); ok {
		t.Error("a GAR image was treated as ECR")
	}
	if _, _, _, ok := parseECRImage("1234.dkr.ecr.eu-west-1.amazonaws.com/purchase"); ok {
		t.Error("an untagged image was accepted")
	}
}

func TestSplitEnvSeparatesLiteralsFromReferences(t *testing.T) {
	literals, secrets := splitEnv([]target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "info"}},
		{Name: "CTP_CLIENT_SECRET", Value: refs.Value{Kind: refs.Secret, Name: "arn:aws:secretsmanager:x"}},
		{Name: "REDIS_URL", Value: refs.Value{Kind: refs.Param, Name: "/platform/redis-url"}},
	})

	if len(literals) != 1 || awssdk.ToString(literals[0].Name) != "LOG_LEVEL" {
		t.Errorf("literals = %+v, want just LOG_LEVEL", literals)
	}
	// Both reference kinds go in `secrets`: valueFrom accepts an SSM parameter
	// just as happily as a Secrets Manager ARN, which is why ECS never needs
	// this tool to read a value.
	if len(secrets) != 2 {
		t.Fatalf("secrets = %+v, want two entries", secrets)
	}
	byName := map[string]string{}
	for _, s := range secrets {
		byName[awssdk.ToString(s.Name)] = awssdk.ToString(s.ValueFrom)
	}
	if byName["CTP_CLIENT_SECRET"] != "arn:aws:secretsmanager:x" {
		t.Errorf("secret valueFrom = %q", byName["CTP_CLIENT_SECRET"])
	}
	if byName["REDIS_URL"] != "/platform/redis-url" {
		t.Errorf("param valueFrom = %q", byName["REDIS_URL"])
	}
}

func container(name, image string, env map[string]string, secrets map[string]string, cpu int32) ecstypes.ContainerDefinition {
	c := ecstypes.ContainerDefinition{
		Name:  awssdk.String(name),
		Image: awssdk.String(image),
		Cpu:   cpu,
	}
	for k, v := range env {
		c.Environment = append(c.Environment, ecstypes.KeyValuePair{
			Name: awssdk.String(k), Value: awssdk.String(v),
		})
	}
	for k, v := range secrets {
		c.Secrets = append(c.Secrets, ecstypes.Secret{
			Name: awssdk.String(k), ValueFrom: awssdk.String(v),
		})
	}
	return c
}

func TestFingerprintIgnoresEnvOrdering(t *testing.T) {
	a := fingerprintContainers([]ecstypes.ContainerDefinition{
		container("app", "repo:v1", map[string]string{"A": "1", "B": "2"}, nil, 256),
	})
	b := fingerprintContainers([]ecstypes.ContainerDefinition{
		container("app", "repo:v1", map[string]string{"B": "2", "A": "1"}, nil, 256),
	})
	if !reflect.DeepEqual(a, b) {
		t.Error("fingerprint changed when env vars were listed in a different order")
	}
}

func TestFingerprintNoticesABaseChange(t *testing.T) {
	// The whole reason the diff is structural rather than a version comparison:
	// Terraform bumping memory produces no new version, so a tag check alone
	// would never apply it.
	before := fingerprintContainers([]ecstypes.ContainerDefinition{
		container("app", "repo:v1", nil, nil, 256),
	})
	after := fingerprintContainers([]ecstypes.ContainerDefinition{
		container("app", "repo:v1", nil, nil, 512),
	})
	if before[0].Rest == after[0].Rest {
		t.Error("a cpu change did not show up in the fingerprint")
	}
}

func TestDiffEnvComparesReferencesByTarget(t *testing.T) {
	have := fingerprintContainers([]ecstypes.ContainerDefinition{
		container("app", "repo:v1",
			map[string]string{"LOG_LEVEL": "info", "GONE": "x"},
			map[string]string{"SECRET": "arn:one"}, 256),
	})[0]
	want := fingerprintContainers([]ecstypes.ContainerDefinition{
		container("app", "repo:v1",
			map[string]string{"LOG_LEVEL": "debug", "NEW": "y"},
			map[string]string{"SECRET": "arn:one"}, 256),
	})[0]

	added, changed, removed := diffEnv(have, want)
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
	// that secret now holds — the tool cannot see the value and should not
	// pretend otherwise.
	for _, name := range changed {
		if name == "SECRET" {
			t.Error("an unchanged reference was reported as changed")
		}
	}
}

func TestADeclaredEnvironmentIsTheWholeEnvironment(t *testing.T) {
	// ECS is the one place the merge could have survived on its own terms: the
	// base family is Terraform's and re-registered whole, so a variable dropped
	// there did reach the next release. It goes anyway, because a config that
	// means the environment on one cloud and a patch over an unseen one on
	// another is not a config anyone can read.
	lits, secs := containerEnv(
		[]ecstypes.KeyValuePair{
			{Name: awssdk.String("LOG_LEVEL"), Value: awssdk.String("info")},
			{Name: awssdk.String("CTP_PROJECT_KEY"), Value: awssdk.String("evolve-tst")},
		},
		[]ecstypes.Secret{
			{Name: awssdk.String("MOLLIE_API_KEY"), ValueFrom: awssdk.String("arn:mollie")},
		},
		[]target.EnvVar{
			{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "debug"}},
			{Name: "FEATURE_QUOTES", Value: refs.Value{Kind: refs.Literal, Literal: "on"}},
		},
		true,
	)

	want := map[string]string{"LOG_LEVEL": "debug", "FEATURE_QUOTES": "on"}
	if got := literalMap(lits); !reflect.DeepEqual(got, want) {
		t.Errorf("literals = %v, want %v", got, want)
	}
	// A reference the config stopped naming goes the same way as a literal.
	// Nothing in ECS distinguishes them once the config is the declaration.
	if len(secs) != 0 {
		t.Errorf("secrets = %v, want them gone with the rest", secretMap(secs))
	}
}

func TestAVariableThatChangesKindLeavesNothingBehind(t *testing.T) {
	// ECS keeps literals and references apart and a name may be in only one of
	// them, so a config that turns a literal into a reference used to have to
	// take it out of the list it was in by hand. Replacing makes that free:
	// splitEnv builds both lists from one declaration and cannot put a name in
	// two of them.
	lits, secs := containerEnv(
		[]ecstypes.KeyValuePair{
			{Name: awssdk.String("CTP_CLIENT_SECRET"), Value: awssdk.String("plaintext")},
		},
		nil,
		[]target.EnvVar{
			{Name: "CTP_CLIENT_SECRET", Value: refs.Value{Kind: refs.Secret, Name: "arn:ctp"}},
		},
		true,
	)

	if len(lits) != 0 {
		t.Errorf("literals = %v, want the one that became a reference taken out", literalMap(lits))
	}
	want := map[string]string{"CTP_CLIENT_SECRET": "arn:ctp"}
	if got := secretMap(secs); !reflect.DeepEqual(got, want) {
		t.Errorf("secrets = %v, want %v", got, want)
	}
}

func TestAConfigDeclaringNoEnvironmentKeepsTheBases(t *testing.T) {
	// A repository that has not moved its environment here yet. Emptying one
	// would be a poor way to tell it so.
	baseLits := []ecstypes.KeyValuePair{
		{Name: awssdk.String("LOG_LEVEL"), Value: awssdk.String("info")},
	}
	baseSecs := []ecstypes.Secret{
		{Name: awssdk.String("MOLLIE_API_KEY"), ValueFrom: awssdk.String("arn:mollie")},
	}

	lits, secs := containerEnv(baseLits, baseSecs, nil, false)

	if !reflect.DeepEqual(lits, baseLits) || !reflect.DeepEqual(secs, baseSecs) {
		t.Errorf("environment was not carried over: %v %v", literalMap(lits), secretMap(secs))
	}
}

func TestRenderTaskDefLeavesSidecarsAlone(t *testing.T) {
	// Only the released container is rewritten. Replacing across the whole
	// definition would empty the collector, whose environment is Terraform's,
	// and retag an image this tool does not own.
	base := &ecstypes.TaskDefinition{
		Cpu:    awssdk.String("512"),
		Memory: awssdk.String("1024"),
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name:  awssdk.String("app"),
				Image: awssdk.String("1234.dkr.ecr.eu-west-1.amazonaws.com/purchase:old"),
				Environment: []ecstypes.KeyValuePair{
					{Name: awssdk.String("STALE"), Value: awssdk.String("yes")},
				},
			},
			{
				Name:  awssdk.String("collector"),
				Image: awssdk.String("1234.dkr.ecr.eu-west-1.amazonaws.com/otel:v9"),
				Environment: []ecstypes.KeyValuePair{
					{Name: awssdk.String("OTEL_SERVICE_NAME"), Value: awssdk.String("purchase")},
				},
			},
		},
	}

	out := renderTaskDef(base, "evolve-tst-purchase", "app",
		"1234.dkr.ecr.eu-west-1.amazonaws.com/purchase:abc1234",
		[]target.EnvVar{
			{Name: "APP_CONFIG_ENDPOINT", Value: refs.Value{Kind: refs.Literal, Literal: "https://store"}},
		}, true)

	app := findContainer(out.ContainerDefinitions, "app")
	want := map[string]string{"APP_CONFIG_ENDPOINT": "https://store"}
	if got := literalMap(app.Environment); !reflect.DeepEqual(got, want) {
		t.Errorf("released container environment = %v, want %v", got, want)
	}

	sidecar := findContainer(out.ContainerDefinitions, "collector")
	if got := literalMap(sidecar.Environment); !reflect.DeepEqual(got, map[string]string{"OTEL_SERVICE_NAME": "purchase"}) {
		t.Errorf("sidecar environment = %v, want it untouched", got)
	}
	if awssdk.ToString(sidecar.Image) != "1234.dkr.ecr.eu-west-1.amazonaws.com/otel:v9" {
		t.Errorf("sidecar image = %q, want it untouched", awssdk.ToString(sidecar.Image))
	}

	// The base is Terraform's and is read again on the next release, so it must
	// come out of this unmodified.
	if got := literalMap(base.ContainerDefinitions[0].Environment); !reflect.DeepEqual(got, map[string]string{"STALE": "yes"}) {
		t.Errorf("the base definition was modified in place: %v", got)
	}
}

func literalMap(lits []ecstypes.KeyValuePair) map[string]string {
	out := make(map[string]string, len(lits))
	for _, kv := range lits {
		out[awssdk.ToString(kv.Name)] = awssdk.ToString(kv.Value)
	}
	return out
}

func secretMap(secs []ecstypes.Secret) map[string]string {
	out := make(map[string]string, len(secs))
	for _, s := range secs {
		out[awssdk.ToString(s.Name)] = awssdk.ToString(s.ValueFrom)
	}
	return out
}
