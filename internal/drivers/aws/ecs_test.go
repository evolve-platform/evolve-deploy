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

func TestMergeEnvKeepsWhatTheConfigDoesNotMention(t *testing.T) {
	// Terraform declares the environment on the base task definition and the
	// config refines it, so a name the config is silent about survives.
	lits, secs := mergeEnv(
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
	)

	want := map[string]string{"LOG_LEVEL": "debug", "CTP_PROJECT_KEY": "evolve-tst", "FEATURE_QUOTES": "on"}
	if got := literalMap(lits); !reflect.DeepEqual(got, want) {
		t.Errorf("literals = %v, want %v", got, want)
	}
	if got := secretMap(secs); !reflect.DeepEqual(got, map[string]string{"MOLLIE_API_KEY": "arn:mollie"}) {
		t.Errorf("secrets = %v, want them untouched", got)
	}
}

func TestMergeEnvMovesANameBetweenTheTwoLists(t *testing.T) {
	// ECS keeps literals and references apart and a name may be in only one of
	// them. A config that turns a literal into a reference has to take it out of
	// the list it was in, or the task definition declares the same name twice.
	lits, secs := mergeEnv(
		[]ecstypes.KeyValuePair{
			{Name: awssdk.String("LOG_LEVEL"), Value: awssdk.String("info")},
			{Name: awssdk.String("CTP_CLIENT_SECRET"), Value: awssdk.String("plaintext")},
		},
		[]ecstypes.Secret{
			{Name: awssdk.String("MOLLIE_API_KEY"), ValueFrom: awssdk.String("arn:mollie")},
		},
		[]target.EnvVar{
			{Name: "CTP_CLIENT_SECRET", Value: refs.Value{Kind: refs.Secret, Name: "arn:ctp"}},
		},
	)

	if got := literalMap(lits); !reflect.DeepEqual(got, map[string]string{"LOG_LEVEL": "info"}) {
		t.Errorf("literals = %v, want the one that became a reference taken out", got)
	}
	want := map[string]string{"MOLLIE_API_KEY": "arn:mollie", "CTP_CLIENT_SECRET": "arn:ctp"}
	if got := secretMap(secs); !reflect.DeepEqual(got, want) {
		t.Errorf("secrets = %v, want %v", got, want)
	}
}

func TestMergeEnvMovesANameBackToALiteral(t *testing.T) {
	// The mirror of the move above, which is a separate branch: a reference the
	// config redeclares as a literal has to leave the secrets list.
	lits, secs := mergeEnv(
		[]ecstypes.KeyValuePair{{Name: awssdk.String("LOG_LEVEL"), Value: awssdk.String("info")}},
		[]ecstypes.Secret{{Name: awssdk.String("CTP_CLIENT_SECRET"), ValueFrom: awssdk.String("arn:ctp")}},
		[]target.EnvVar{
			{Name: "CTP_CLIENT_SECRET", Value: refs.Value{Kind: refs.Literal, Literal: "plaintext"}},
		},
	)

	want := map[string]string{"LOG_LEVEL": "info", "CTP_CLIENT_SECRET": "plaintext"}
	if got := literalMap(lits); !reflect.DeepEqual(got, want) {
		t.Errorf("literals = %v, want %v", got, want)
	}
	if len(secs) != 0 {
		t.Errorf("secrets = %v, want the name gone from there", secretMap(secs))
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
