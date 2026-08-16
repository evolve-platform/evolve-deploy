package azure

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

func container(name, image string, env ...*armappcontainers.EnvironmentVar) *armappcontainers.Container {
	return &armappcontainers.Container{
		Name:  to.Ptr(name),
		Image: to.Ptr(image),
		Env:   env,
	}
}

func literal(name, value string) *armappcontainers.EnvironmentVar {
	return &armappcontainers.EnvironmentVar{Name: to.Ptr(name), Value: to.Ptr(value)}
}

func secretRef(name, secret string) *armappcontainers.EnvironmentVar {
	return &armappcontainers.EnvironmentVar{Name: to.Ptr(name), SecretRef: to.Ptr(secret)}
}

func TestSidecarsAreLeftAlone(t *testing.T) {
	// Both app-container and app-container-job put a reverse proxy next to the
	// application. Its image is Terraform's — retagging it with the
	// application's version would deploy a proxy that does not exist.
	current := []*armappcontainers.Container{
		container("main", "reg.azurecr.io/purchase:old",
			literal("LOG_LEVEL", "info")),
		container("reverse-proxy", "reg.azurecr.io/proxy:v9",
			literal("OTEL_SERVICE_NAME", "purchase")),
	}

	next, from, err := nextContainers(current, "main", "abc1234", []target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "debug"}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	if from != "old" {
		t.Errorf("from = %q, want the tag that was running", from)
	}
	if got := derefString(findContainer(next, "main").Image); got != "reg.azurecr.io/purchase:abc1234" {
		t.Errorf("application image = %q", got)
	}

	proxy := findContainer(next, "reverse-proxy")
	if got := derefString(proxy.Image); got != "reg.azurecr.io/proxy:v9" {
		t.Errorf("the sidecar image was changed to %q", got)
	}
	if got := envFingerprint(proxy.Env)["OTEL_SERVICE_NAME"]; got != "=purchase" {
		t.Errorf("the sidecar environment was changed: %q", got)
	}
}

func TestReferencesBecomeSecretRefs(t *testing.T) {
	// On Azure a secret is declared on the resource by Terraform and referred
	// to by name, so the tool writes a secretRef and never sees a value.
	got := renderEnv([]target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "info"}},
		{Name: "CTP_CLIENT_SECRET", Value: refs.Value{Kind: refs.Secret, Name: "ctp-client-secret"}},
	})

	fp := envFingerprint(got)
	if fp["LOG_LEVEL"] != "=info" {
		t.Errorf("LOG_LEVEL = %q", fp["LOG_LEVEL"])
	}
	if fp["CTP_CLIENT_SECRET"] != "->ctp-client-secret" {
		t.Errorf("CTP_CLIENT_SECRET = %q, want a secretRef", fp["CTP_CLIENT_SECRET"])
	}
	for _, e := range got {
		if derefString(e.Name) == "CTP_CLIENT_SECRET" && e.Value != nil {
			t.Error("a secret reference was written out as a literal value")
		}
	}
}

func TestVerifySecretRefs(t *testing.T) {
	declared := []*armappcontainers.Secret{
		{Name: to.Ptr("ctp-client-secret")},
		{Name: to.Ptr("mollie-api-key")},
	}

	ok := []target.EnvVar{
		{Name: "CTP_CLIENT_SECRET", Value: refs.Value{Kind: refs.Secret, Name: "ctp-client-secret"}},
	}
	if err := verifySecretRefs(ok, declared, "container app purchase"); err != nil {
		t.Errorf("a declared secret was rejected: %v", err)
	}

	// Catching this at plan time matters: the deploy would otherwise succeed
	// and the revision would fail to start.
	bad := []target.EnvVar{
		{Name: "STRIPE_KEY", Value: refs.Value{Kind: refs.Secret, Name: "stripe-key"}},
	}
	err := verifySecretRefs(bad, declared, "container app purchase")
	if err == nil {
		t.Fatal("an undeclared secret was accepted")
	}
	if !strings.Contains(err.Error(), "stripe-key") ||
		!strings.Contains(err.Error(), "ctp-client-secret") {
		t.Errorf("the error should name what is missing and what is there: %v", err)
	}
}

func TestDiffContainersIgnoresSidecarsAndRotations(t *testing.T) {
	current := []*armappcontainers.Container{
		container("main", "reg/purchase:old",
			literal("LOG_LEVEL", "info"),
			literal("GONE", "x"),
			secretRef("CTP_CLIENT_SECRET", "ctp-client-secret")),
		container("reverse-proxy", "reg/proxy:v9", literal("SIDECAR_ONLY", "y")),
	}
	next := []*armappcontainers.Container{
		container("main", "reg/purchase:new",
			literal("LOG_LEVEL", "debug"),
			literal("NEW", "y"),
			secretRef("CTP_CLIENT_SECRET", "ctp-client-secret")),
		container("reverse-proxy", "reg/proxy:v9", literal("SIDECAR_ONLY", "y")),
	}

	added, changed, removed := diffContainers(current, next, "main")
	if !reflect.DeepEqual(added, []string{"NEW"}) {
		t.Errorf("added = %v", added)
	}
	if !reflect.DeepEqual(changed, []string{"LOG_LEVEL"}) {
		t.Errorf("changed = %v", changed)
	}
	if !reflect.DeepEqual(removed, []string{"GONE"}) {
		t.Errorf("removed = %v", removed)
	}
	// The sidecar's variables are not this tool's business and must not show up
	// as a change. A reference still pointing at the same secret has not
	// changed either, whatever that secret now holds.
	for _, list := range [][]string{added, changed, removed} {
		for _, name := range list {
			if name == "SIDECAR_ONLY" || name == "CTP_CLIENT_SECRET" {
				t.Errorf("%s was reported as changed", name)
			}
		}
	}
}

func TestNextContainersRefusesAnUntaggedImage(t *testing.T) {
	_, _, err := nextContainers(
		[]*armappcontainers.Container{container("main", "")}, "main", "abc", nil, true)
	if err == nil {
		t.Fatal("an image with no repository was accepted")
	}
}

func TestAnAbsentEnvConfigLeavesTheEnvironmentAlone(t *testing.T) {
	// A config that mentions no variables is asking for an image-only deploy.
	// Writing an empty list would delete everything Terraform set.
	current := []*armappcontainers.Container{
		container("main", "reg/purchase:old",
			literal("CTP_CLIENT_ID", "abc"),
			secretRef("CTP_CLIENT_SECRET", "ctp-client-secret")),
	}

	next, _, err := nextContainers(current, "main", "abc1234", nil, false)
	if err != nil {
		t.Fatal(err)
	}

	got := envFingerprint(findContainer(next, "main").Env)
	if len(got) != 2 {
		t.Fatalf("environment = %v, want it carried over untouched", got)
	}
	if got["CTP_CLIENT_ID"] != "=abc" || got["CTP_CLIENT_SECRET"] != "->ctp-client-secret" {
		t.Errorf("environment = %v", got)
	}
	// The image still moves — that is the whole point of an image-only deploy.
	if img := derefString(findContainer(next, "main").Image); img != "reg/purchase:abc1234" {
		t.Errorf("image = %q", img)
	}
}
