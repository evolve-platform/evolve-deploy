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
	}, true, nil)
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
		[]*armappcontainers.Container{container("main", "")}, "main", "abc", nil, false, nil)
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

	next, _, err := nextContainers(current, "main", "abc1234", nil, false, nil)
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
	detail, container, crashing := describeReplicas([]*armappcontainers.Replica{
		replica(
			replicaContainer("main", false,
				armappcontainers.ContainerAppContainerRunningStateWaiting,
				"Container is waiting with reason: CrashLoopBackOff on legion.",
				crashLoopRestarts),
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
	// What Container Apps writes is meant for a portal blade: a prefix that
	// repeats the state beside it, and the name of the node that ran the thing.
	// Neither survives being read at the end of a long error line.
	if strings.Contains(detail, "waiting with reason") || strings.Contains(detail, "legion") {
		t.Errorf("the platform's boilerplate reached the error: %q", detail)
	}
	// The logs of the container that is dying are the next thing to look at, so
	// the command that shows them has to be able to name it.
	if container != "main" {
		t.Errorf("container = %q, want the one that is crashing", container)
	}
}

func TestACrashLoopIsAVerdictWhateverTheRevisionSays(t *testing.T) {
	// The case this exists for: Container Apps leaves the revision in Processing
	// while the container inside it dies and is restarted, so the revision
	// itself never says anything and the wait ran to readyTimeout -- ten minutes
	// for a process that was gone in ten seconds. The restart count is a level
	// down from where the revision reports, and it outranks the silence.
	reason, certain := weighReplicas("", true)
	if !certain {
		t.Error("a crash loop under a quiet revision was not acted on")
	}
	if reason != "never started" {
		t.Errorf("reason = %q, want what the container actually did", reason)
	}

	// The same fact also replaces what the revision does say, because "is
	// Unhealthy" is a state and "never started" is the reason for it.
	if reason, _ := weighReplicas("is Degraded", true); reason != "never started" {
		t.Errorf("reason = %q, want the container's account over the revision's", reason)
	}

	// And with nothing crashing, silence stays silence: a container that is not
	// up and has not died is one that is starting, and a strike here would end a
	// release that was going to work after fifteen seconds.
	if reason, certain := weighReplicas("", false); reason != "" || certain {
		t.Errorf("a starting container produced a verdict: %q %v", reason, certain)
	}
	// A revision that reported badly is still only a suspicion on its own.
	if reason, certain := weighReplicas("is Degraded", false); reason != "is Degraded" || certain {
		t.Errorf("weighReplicas = %q %v, want the revision's suspicion unchanged", reason, certain)
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

	detail, _, crashing := describeReplicas([]*armappcontainers.Replica{one(), one(), one()})
	if crashing {
		t.Error("a single restart was called a crash loop")
	}
	if n := strings.Count(detail, "exited with code 1"); n != 1 {
		t.Errorf("the same failure was reported %d times: %q", n, detail)
	}
	if !strings.Contains(detail, "restarted once") {
		t.Errorf("the restart count is missing: %q", detail)
	}
}

func TestAHealthyRevisionHasNothingToDescribe(t *testing.T) {
	detail, _, crashing := describeReplicas([]*armappcontainers.Replica{
		replica(replicaContainer("main", true,
			armappcontainers.ContainerAppContainerRunningStateRunning, "", 0)),
		// A replica with nothing in it must not panic anything.
		{},
	})
	if detail != "" || crashing {
		t.Errorf("a healthy revision produced %q (crashing=%v)", detail, crashing)
	}
}

func TestADeclaredEnvironmentIsTheWholeEnvironment(t *testing.T) {
	// The config is the declaration, so a name it does not mention is gone.
	// Laying it over what was there instead could never remove anything, and on
	// Azure nothing else can either -- Terraform has `env` on ignore_changes, so a
	// variable it wrote at create outlived every release and went on outranking
	// whatever was meant to replace it.
	current := []*armappcontainers.Container{
		container("main", "reg/purchase:old",
			literal("LOG_LEVEL", "info"),
			literal("CTP_PROJECT_KEY", "evolve-tst"),
			secretRef("CTP_CLIENT_SECRET", "ctp-client-secret")),
	}

	next, _, err := nextContainers(current, "main", "abc1234", []target.EnvVar{
		{Name: "LOG_LEVEL", Value: refs.Value{Kind: refs.Literal, Literal: "debug"}},
		{Name: "FEATURE_QUOTES", Value: refs.Value{Kind: refs.Literal, Literal: "on"}},
	}, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	got := envFingerprint(findContainer(next, "main").Env, nil)
	want := map[string]string{
		"LOG_LEVEL":      "=debug",
		"FEATURE_QUOTES": "=on",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("environment = %v, want %v", got, want)
	}
}

func TestAVariableThatChangesKindLeavesNothingBehind(t *testing.T) {
	// A literal that becomes a reference is the same name twice if the merge
	// appends instead of replacing, and Container Apps then resolves whichever
	// comes last — a value decided by an ordering nobody declared.
	current := []*armappcontainers.Container{
		container("main", "reg/purchase:old", literal("CTP_CLIENT_SECRET", "plaintext")),
	}

	next, _, err := nextContainers(current, "main", "abc1234", []target.EnvVar{
		{Name: "CTP_CLIENT_SECRET", Value: refs.Value{Kind: refs.Secret, Name: "ctp-client-secret"}},
	}, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	env := findContainer(next, "main").Env
	if len(env) != 1 {
		t.Fatalf("environment has %d variables, want the one it started with", len(env))
	}
	if got := derefString(env[0].SecretRef); got != "ctp-client-secret" {
		t.Errorf("secretRef = %q", got)
	}
	if env[0].Value != nil {
		t.Error("the literal value survived alongside the reference")
	}
}

func TestTheConfigCannotRemoveAVariable(t *testing.T) {
	// The other half of merging, written down: dropping a name from the deploy
	// config leaves it where it was. Removal belongs where the declaration is,
	// and a config that could delete would delete everything Terraform set and
	// the config never mentioned.
	current := []*armappcontainers.Container{
		container("main", "reg/purchase:old", literal("LOG_LEVEL", "info")),
	}

	next, _, err := nextContainers(current, "main", "abc1234", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := envFingerprint(findContainer(next, "main").Env, nil)["LOG_LEVEL"]; got != "=info" {
		t.Errorf("LOG_LEVEL = %q, want it left alone", got)
	}
}

func TestTheSameConfigTwiceWritesTheSameList(t *testing.T) {
	// A template that differs is a revision ARM has to create, so the order has
	// to be stable across runs.
	base := []*armappcontainers.EnvironmentVar{
		literal("A", "1"), literal("B", "2"), secretRef("C", "c"),
	}
	declared := renderEnv([]target.EnvVar{
		{Name: "B", Value: refs.Value{Kind: refs.Literal, Literal: "two"}},
		{Name: "Z", Value: refs.Value{Kind: refs.Literal, Literal: "z"}},
		{Name: "Y", Value: refs.Value{Kind: refs.Literal, Literal: "y"}},
	})

	var first []string
	for i := 0; i < 5; i++ {
		got := containerEnv(base, declared, true)
		names := make([]string, 0, len(got))
		for _, e := range got {
			names = append(names, derefString(e.Name))
		}
		if first == nil {
			first = names
			continue
		}
		if !reflect.DeepEqual(names, first) {
			t.Fatalf("order changed between runs: %v then %v", first, names)
		}
	}
	if want := []string{"B", "Z", "Y"}; !reflect.DeepEqual(first, want) {
		t.Errorf("names = %v, want %v", first, want)
	}
}

func TestADeclaredEnvironmentRemovesWhatItDoesNotName(t *testing.T) {
	// The reason this changed: nothing else can remove a variable. Terraform has
	// `env` on ignore_changes, so one it wrote at create outlived every release
	// and went on outranking whatever was meant to replace it.
	base := []*armappcontainers.EnvironmentVar{
		literal("CTP_API_URL", "https://old.example"),
		secretRef("CTP_CLIENT_SECRET", "ctp-client-secret"),
	}
	declared := renderEnv([]target.EnvVar{
		{Name: "APP_CONFIG_ENDPOINT", Value: refs.Value{Kind: refs.Literal, Literal: "https://store"}},
	})

	got := containerEnv(base, declared, true)

	names := make([]string, 0, len(got))
	for _, e := range got {
		names = append(names, derefString(e.Name))
	}
	if want := []string{"APP_CONFIG_ENDPOINT"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestAConfigDeclaringNoEnvironmentKeepsWhatIsThere(t *testing.T) {
	// A repository that has not moved its environment here yet. Emptying one
	// would be a poor way to tell it so.
	base := []*armappcontainers.EnvironmentVar{literal("A", "1"), secretRef("B", "b")}

	got := containerEnv(base, nil, false)

	if !reflect.DeepEqual(got, base) {
		t.Errorf("environment was not carried over: %v", got)
	}
}

func TestASidecarKeepsItsOwnEnvironment(t *testing.T) {
	// A sidecar's image and environment are Terraform's. The gateway runs one, so
	// replacing across the whole template would empty the reverse proxy.
	current := []*armappcontainers.Container{
		{
			Name:  to.Ptr("main"),
			Image: to.Ptr("registry.example/app:old"),
			Env:   []*armappcontainers.EnvironmentVar{literal("STALE", "yes")},
		},
		{
			Name:  to.Ptr("reverse-proxy"),
			Image: to.Ptr("registry.example/proxy:pinned"),
			Env:   []*armappcontainers.EnvironmentVar{literal("OTEL_SERVICE_NAME", "proxy")},
		},
	}
	declared := []target.EnvVar{
		{Name: "APP_CONFIG_ENDPOINT", Value: refs.Value{Kind: refs.Literal, Literal: "https://store"}},
	}

	next, _, err := nextContainers(current, "main", "new", declared, true, nil)
	if err != nil {
		t.Fatalf("nextContainers: %v", err)
	}

	proxy := findContainer(next, "reverse-proxy")
	if proxy == nil || len(proxy.Env) != 1 || derefString(proxy.Env[0].Name) != "OTEL_SERVICE_NAME" {
		t.Errorf("sidecar environment was rewritten: %+v", proxy)
	}
	if derefString(proxy.Image) != "registry.example/proxy:pinned" {
		t.Errorf("sidecar image was retagged: %s", derefString(proxy.Image))
	}

	main := findContainer(next, "main")
	if main == nil || len(main.Env) != 1 || derefString(main.Env[0].Name) != "APP_CONFIG_ENDPOINT" {
		t.Errorf("released container env = %+v, want only APP_CONFIG_ENDPOINT", main)
	}
}

// Container Apps follows Kubernetes, where args are appended to command. The
// config's `command` is the whole line, so args left from what Terraform
// declared would extend a command line the config meant to replace.
func TestADeclaredCommandReplacesTheEntryPointAndTheArgsWithIt(t *testing.T) {
	current := []*armappcontainers.Container{{
		Name:    to.Ptr("main"),
		Image:   to.Ptr("evolve.azurecr.io/discover:old"),
		Command: to.SliceOfPtrs("node", "cli.mjs"),
		Args:    to.SliceOfPtrs("sync", "products"),
	}}

	next, from, err := nextContainers(current, "main", "abc1234", nil, false,
		[]string{"node", "src/cli.ts", "sync", "products"})
	if err != nil {
		t.Fatal(err)
	}
	if from != "old" {
		t.Errorf("from = %q", from)
	}

	c := findContainer(next, "main")
	if got := strings.Join(containerCommand(c), " "); got != "node src/cli.ts sync products" {
		t.Errorf("command = %q", got)
	}
	if len(c.Args) != 0 {
		t.Errorf("args = %v, want them dropped with the command they belonged to", c.Args)
	}
	if got := strings.Join(diffCommand(current, next, "main"), " "); got != "node src/cli.ts sync products" {
		t.Errorf("diffCommand = %q", got)
	}
}

// Absent means absent: a config written before this existed goes on deploying
// the image and nothing else, with Terraform still owning the entry point.
func TestNoDeclaredCommandLeavesTheEntryPointAlone(t *testing.T) {
	current := []*armappcontainers.Container{{
		Name:    to.Ptr("main"),
		Image:   to.Ptr("evolve.azurecr.io/discover:old"),
		Command: to.SliceOfPtrs("node", "cli.mjs", "sync", "products"),
	}}

	next, _, err := nextContainers(current, "main", "abc1234", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := findContainer(next, "main")
	if got := strings.Join(containerCommand(c), " "); got != "node cli.mjs sync products" {
		t.Errorf("command = %q, want the one Terraform set", got)
	}
	if got := diffCommand(current, next, "main"); got != nil {
		t.Errorf("diffCommand = %v, want nothing to report", got)
	}
}

// A command that moved while the version did not is still a change, and the
// plan has to say which of the two things a deploy owns moved.
func TestACommandOnlyChangeIsNamedInTheReason(t *testing.T) {
	if got := reason("abc1234", "abc1234", false, true); got != "command changed" {
		t.Errorf("reason = %q", got)
	}
	if got := reason("abc1234", "abc1234", true, true); got != "environment and command changed" {
		t.Errorf("reason = %q", got)
	}
	if got := reason("abc1234", "abc1234", true, false); got != "environment changed" {
		t.Errorf("reason = %q", got)
	}
}
