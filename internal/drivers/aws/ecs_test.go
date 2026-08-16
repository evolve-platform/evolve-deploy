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
