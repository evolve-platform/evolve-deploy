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
	if got := envFingerprint(proxy.Env, nil)["OTEL_SERVICE_NAME"]; got != "=purchase" {
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

	fp := envFingerprint(got, nil)
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

	added, changed, removed := diffContainers(current, next, "main", nil)
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

	got := envFingerprint(findContainer(next, "main").Env, nil)
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

func replica(containers ...*armappcontainers.ReplicaContainer) *armappcontainers.Replica {
	return &armappcontainers.Replica{
		Properties: &armappcontainers.ReplicaProperties{Containers: containers},
	}
}

func replicaContainer(
	name string, ready bool, state armappcontainers.ContainerAppContainerRunningState,
	details string, restarts int32,
) *armappcontainers.ReplicaContainer {
	return &armappcontainers.ReplicaContainer{
		Name:                to.Ptr(name),
		Ready:               to.Ptr(ready),
		RunningState:        to.Ptr(state),
		RunningStateDetails: to.Ptr(details),
		RestartCount:        to.Ptr(restarts),
	}
}

func TestARevisionStillStartingIsNotAFailure(t *testing.T) {
	// The whole point of the strike count is that a slow start must not be read
	// as a broken one. Processing is what a revision reports for as long as it
	// is coming up, and it has to classify as "keep waiting" — anything else
	// rolls back deploys that were going to work.
	props := &armappcontainers.RevisionProperties{
		ProvisioningState: to.Ptr(armappcontainers.RevisionProvisioningStateProvisioning),
		RunningState:      to.Ptr(armappcontainers.RevisionRunningStateProcessing),
		HealthState:       to.Ptr(armappcontainers.RevisionHealthStateNone),
	}
	if reason, _ := classifyRevision(props); reason != "" {
		t.Errorf("a revision that is still starting was called broken: %q", reason)
	}

	// Nor may a healthy one, obviously.
	props = &armappcontainers.RevisionProperties{
		ProvisioningState: to.Ptr(armappcontainers.RevisionProvisioningStateProvisioned),
		RunningState:      to.Ptr(armappcontainers.RevisionRunningStateRunning),
		HealthState:       to.Ptr(armappcontainers.RevisionHealthStateHealthy),
	}
	if reason, _ := classifyRevision(props); reason != "" {
		t.Errorf("a healthy revision was called broken: %q", reason)
	}

	// And a response with nothing in it is not evidence of anything.
	if reason, certain := classifyRevision(nil); reason != "" || certain {
		t.Errorf("an empty revision produced a verdict: %q %v", reason, certain)
	}
}

func TestAFailedProvisionIsActedOnAtOnce(t *testing.T) {
	// This is the case that used to cost ten minutes: the platform has already
	// given up, and there is nothing to wait for. The message it carries is
	// usually the whole answer, so it has to reach the error.
	props := &armappcontainers.RevisionProperties{
		ProvisioningState: to.Ptr(armappcontainers.RevisionProvisioningStateFailed),
		ProvisioningError: to.Ptr("manifest unknown: manifest tagged by \"3f02cd4\" is not found"),
	}

	reason, certain := classifyRevision(props)
	if !certain {
		t.Error("a failed provision should not need a second opinion")
	}
	if !strings.Contains(reason, "manifest unknown") {
		t.Errorf("the platform's own message was dropped: %q", reason)
	}
}

func TestADegradedRevisionIsOnlySuspicion(t *testing.T) {
	// Container Apps restarts a failing container, so Degraded and Processing
	// alternate. Acting on the first sighting would fail deploys that recover.
	for _, state := range []armappcontainers.RevisionRunningState{
		armappcontainers.RevisionRunningStateDegraded,
		armappcontainers.RevisionRunningStateFailed,
	} {
		props := &armappcontainers.RevisionProperties{RunningState: to.Ptr(state)}
		reason, certain := classifyRevision(props)
		if reason == "" {
			t.Errorf("%s was not reported at all", state)
		}
		if certain {
			t.Errorf("%s was treated as final on one poll", state)
		}
	}
}

func TestRestartsTurnSuspicionIntoAVerdict(t *testing.T) {
	// A container that has already died crashLoopRestarts times is not slow.
	detail, crashing := describeReplicas([]*armappcontainers.Replica{
		replica(
			replicaContainer("main", false,
				armappcontainers.ContainerAppContainerRunningStateWaiting,
				"CrashLoopBackOff", crashLoopRestarts),
			// The sidecar is up and has nothing to do with this.
			replicaContainer("reverse-proxy", true,
				armappcontainers.ContainerAppContainerRunningStateRunning, "", 0),
		),
	})

	if !crashing {
		t.Error("a container past the restart limit was not called a crash loop")
	}
	if !strings.Contains(detail, "main") || !strings.Contains(detail, "CrashLoopBackOff") {
		t.Errorf("the detail does not say what broke: %q", detail)
	}
	if strings.Contains(detail, "reverse-proxy") {
		t.Errorf("a container that is up was reported as a problem: %q", detail)
	}
}

func TestOneSentencePerBrokenContainer(t *testing.T) {
	// Every replica of a broken revision reports the same thing, and three
	// copies of one sentence is not three times the information.
	one := func() *armappcontainers.Replica {
		return replica(replicaContainer("main", false,
			armappcontainers.ContainerAppContainerRunningStateTerminated,
			"Container exited with code 1", 1))
	}

	detail, crashing := describeReplicas([]*armappcontainers.Replica{one(), one(), one()})
	if crashing {
		t.Error("a single restart was called a crash loop")
	}
	if n := strings.Count(detail, "exited with code 1"); n != 1 {
		t.Errorf("the same failure was reported %d times: %q", n, detail)
	}
	if !strings.Contains(detail, "1 restart(s)") {
		t.Errorf("the restart count is missing: %q", detail)
	}
}

func TestAHealthyRevisionHasNothingToDescribe(t *testing.T) {
	detail, crashing := describeReplicas([]*armappcontainers.Replica{
		replica(replicaContainer("main", true,
			armappcontainers.ContainerAppContainerRunningStateRunning, "", 0)),
		// A replica with nothing in it must not panic anything.
		{},
	})
	if detail != "" || crashing {
		t.Errorf("a healthy revision produced %q (crashing=%v)", detail, crashing)
	}
}
