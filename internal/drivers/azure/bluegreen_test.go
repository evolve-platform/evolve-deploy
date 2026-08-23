package azure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

func tw(label, revision string, weight int32) *armappcontainers.TrafficWeight {
	w := &armappcontainers.TrafficWeight{Weight: to.Ptr(weight)}
	if label != "" {
		w.Label = to.Ptr(label)
	}
	if revision != "" {
		w.RevisionName = to.Ptr(revision)
	}
	return w
}

func latest(label string, weight int32) *armappcontainers.TrafficWeight {
	return &armappcontainers.TrafficWeight{
		Label:          to.Ptr(label),
		LatestRevision: to.Ptr(true),
		Weight:         to.Ptr(weight),
	}
}

var labels = []string{"blue", "green"}

// The refusal table: active is the label with 100% of the traffic, and every
// other shape is a refusal rather than a guess.
func TestReadSides(t *testing.T) {
	cases := []struct {
		name    string
		traffic []*armappcontainers.TrafficWeight
		active  string
		idle    string
		// idleRevision is what the idle label already points at, if anything.
		idleRevision string
		pin          bool
		wantErr      string
	}{{
		name:    "one side, the app has only ever been deployed once",
		traffic: []*armappcontainers.TrafficWeight{tw("blue", "app--a1", 100)},
		active:  "blue",
		idle:    "green",
	}, {
		name: "the resting state after a deploy",
		traffic: []*armappcontainers.TrafficWeight{
			tw("green", "app--b2", 100),
			tw("blue", "app--a1", 0),
		},
		active:       "green",
		idle:         "blue",
		idleRevision: "app--a1",
	}, {
		name: "a split is refused",
		traffic: []*armappcontainers.TrafficWeight{
			tw("blue", "app--a1", 80),
			tw("green", "app--b2", 20),
		},
		wantErr: "traffic is split",
	}, {
		name:    "100% on an unlabelled entry has nothing to fall back to",
		traffic: []*armappcontainers.TrafficWeight{tw("", "app--a1", 100)},
		wantErr: "no label",
	}, {
		name:    "a label the config does not know is refused",
		traffic: []*armappcontainers.TrafficWeight{tw("live", "app--a1", 100)},
		wantErr: `is "live"`,
	}, {
		name:    "an empty traffic block is refused",
		traffic: nil,
		wantErr: "traffic is split",
	}, {
		name:    "latestRevision is a bootstrap that needs pinning",
		traffic: []*armappcontainers.TrafficWeight{latest("blue", 100)},
		active:  "blue",
		idle:    "green",
		pin:     true,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sides, err := readSides(c.traffic, labels)

			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("no error, want one mentioning %q", c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error = %v, want it to mention %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sides.Active.Label != c.active {
				t.Errorf("active = %q, want %q", sides.Active.Label, c.active)
			}
			if sides.Idle.Label != c.idle {
				t.Errorf("idle = %q, want %q", sides.Idle.Label, c.idle)
			}
			if sides.Idle.Revision != c.idleRevision {
				t.Errorf("idle revision = %q, want %q", sides.Idle.Revision, c.idleRevision)
			}
			if sides.PinNeeded != c.pin {
				t.Errorf("PinNeeded = %v, want %v", sides.PinNeeded, c.pin)
			}
		})
	}
}

// A refusal has to show what was actually there, or whoever reads it in a
// pipeline log has no idea what to fix.
func TestRefusalShowsTheTrafficBlock(t *testing.T) {
	_, err := readSides([]*armappcontainers.TrafficWeight{
		tw("blue", "app--a1", 80),
		tw("green", "app--b2", 20),
	}, labels)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"blue", "app--a1", "80%", "green", "app--b2", "20%", "--to blue"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
}

func TestWeightsLeavesOutASideWithNoRevision(t *testing.T) {
	// The idle label of an app deployed only once points at nothing, and a
	// label cannot point at nothing.
	got := weights(
		trafficEntry{target.Side{Label: "blue", Revision: "app--a1"}, weightAll},
		trafficEntry{target.Side{Label: "green"}, weightNone},
	)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want only the one that has a revision", len(got))
	}
	if derefString(got[0].Label) != "blue" || derefInt32(got[0].Weight) != 100 {
		t.Errorf("entry = %+v", *got[0])
	}
}

func TestWeightsDropsTheLatestRule(t *testing.T) {
	// Everything this tool writes is an explicit revision. If a weight ever
	// went out still saying "whatever is newest", the next revision created
	// would take all the traffic before the smoke test existed.
	for _, w := range weights(
		trafficEntry{target.Side{Label: "blue", Revision: "app--a1"}, weightAll},
		trafficEntry{target.Side{Label: "green", Revision: "app--b2"}, weightNone},
	) {
		if w.LatestRevision != nil {
			t.Errorf("%s carries latestRevision", derefString(w.Label))
		}
		if w.RevisionName == nil {
			t.Errorf("%s names no revision", derefString(w.Label))
		}
	}
}

func TestPointTraffic(t *testing.T) {
	current := []*armappcontainers.TrafficWeight{
		tw("blue", "app--a1", 80),
		tw("green", "app--b2", 20),
	}

	got, err := pointTraffic(current, "blue")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int32{"blue": 100, "green": 0}
	for _, w := range got {
		if got := derefInt32(w.Weight); got != want[derefString(w.Label)] {
			t.Errorf("%s = %d%%, want %d%%", derefString(w.Label), got, want[derefString(w.Label)])
		}
	}

	// The state this command exists to repair is the one Sides refuses to
	// interpret, so it must work on a split rather than reject it.
	if _, err := pointTraffic(current, "purple"); err == nil {
		t.Error("pointing at a label that is not there should fail")
	} else if !strings.Contains(err.Error(), "blue, green") {
		t.Errorf("error should list what is there, got %v", err)
	}
}

func TestPointTrafficUnpins(t *testing.T) {
	got, err := pointTraffic([]*armappcontainers.TrafficWeight{latest("blue", 100)}, "blue")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LatestRevision != nil {
		t.Errorf("latestRevision survived: %+v", got)
	}
}

func TestLabelURL(t *testing.T) {
	cases := []struct{ fqdn, label, want string }{
		{"evolve-tst-site.happyhill-1.westeurope.azurecontainerapps.io", "green",
			"https://evolve-tst-site---green.happyhill-1.westeurope.azurecontainerapps.io"},
		{"", "green", ""},
		{"nodots", "green", ""},
	}
	for _, c := range cases {
		if got := labelURL(c.fqdn, c.label); got != c.want {
			t.Errorf("labelURL(%q, %q) = %q, want %q", c.fqdn, c.label, got, c.want)
		}
	}
}

// The side is written but never compared. Both halves matter: without the write
// a router cannot tell which graph it belongs to, and without the exclusion
// every plan reports a change, so every run deploys and the sides flip forever.
func TestSideIsWrittenButNotCompared(t *testing.T) {
	current := []*armappcontainers.Container{
		container("main", "reg.azurecr.io/site:old", literal("LOG_LEVEL", "info")),
	}

	next := withSide(current, "main", "green", nil, nil)

	got := findContainer(next, "main")
	var found string
	for _, e := range got.Env {
		if derefString(e.Name) == target.SideEnvVar {
			found = derefString(e.Value)
		}
	}
	if found != "green" {
		t.Errorf("%s = %q, want green", target.SideEnvVar, found)
	}

	if _, ok := envFingerprint(got.Env, nil)[target.SideEnvVar]; ok {
		t.Errorf("%s is part of the fingerprint, so it would be diffed", target.SideEnvVar)
	}
	if added, changed, removed := diffContainers(current, next, "main", nil); len(added)+len(changed)+len(removed) != 0 {
		t.Errorf("writing the side produced a diff: +%v ~%v -%v", added, changed, removed)
	}
}

func TestWithSideReplacesAnExistingValue(t *testing.T) {
	// Every release flips it, so the second one must not end up with two.
	current := []*armappcontainers.Container{
		container("main", "reg.azurecr.io/site:old",
			literal(target.SideEnvVar, "blue"), literal("LOG_LEVEL", "info")),
	}

	got := findContainer(withSide(current, "main", "green", nil, nil), "main")

	var n int
	for _, e := range got.Env {
		if derefString(e.Name) == target.SideEnvVar {
			n++
			if derefString(e.Value) != "green" {
				t.Errorf("value = %q, want green", derefString(e.Value))
			}
		}
	}
	if n != 1 {
		t.Errorf("%s appears %d times", target.SideEnvVar, n)
	}
	if len(got.Env) != 2 {
		t.Errorf("env has %d entries, want LOG_LEVEL and the side", len(got.Env))
	}
}

func TestWithSideLeavesSidecarsAlone(t *testing.T) {
	current := []*armappcontainers.Container{
		container("main", "reg.azurecr.io/site:old"),
		container("reverse-proxy", "reg.azurecr.io/proxy:v9", literal("OTEL_SERVICE_NAME", "site")),
	}

	proxy := findContainer(withSide(current, "main", "green", nil, nil), "reverse-proxy")
	for _, e := range proxy.Env {
		if derefString(e.Name) == target.SideEnvVar {
			t.Error("the sidecar was given the side variable")
		}
	}
	if len(proxy.Env) != 1 {
		t.Errorf("the sidecar environment changed: %d entries", len(proxy.Env))
	}
}

// The staged containers are copied from the serving revision, so the side's own
// variables arrive carrying the *other* side's values. Overwriting them is the
// whole point of passing the managed set separately.
func TestSideEnvReplacesTheOtherSidesValues(t *testing.T) {
	current := []*armappcontainers.Container{
		container("main", "reg.azurecr.io/gateway:old",
			literal(target.SideEnvVar, "blue"),
			literal("HIVE_CDN_ENDPOINT", "https://cdn/artifacts/v1/tst-blue"),
			secretRef("HIVE_CDN_KEY", "hive-cdn-blue"),
			literal("LOG_LEVEL", "info"),
		),
	}

	sideEnv := []target.EnvVar{
		{Name: "HIVE_CDN_ENDPOINT", Value: refs.Value{
			Kind: refs.Literal, Literal: "https://cdn/artifacts/v1/tst-green"}},
		{Name: "HIVE_CDN_KEY", Value: refs.Value{Kind: refs.Secret, Name: "hive-cdn-green"}},
	}
	managed := []string{"HIVE_CDN_ENDPOINT", "HIVE_CDN_KEY"}

	got := findContainer(withSide(current, "main", "green", sideEnv, managed), "main")

	fp := envFingerprint(got.Env, nil)
	if fp["HIVE_CDN_ENDPOINT"] != "=https://cdn/artifacts/v1/tst-green" {
		t.Errorf("endpoint = %q, want the green one", fp["HIVE_CDN_ENDPOINT"])
	}
	if fp["HIVE_CDN_KEY"] != "->hive-cdn-green" {
		t.Errorf("key = %q, want the green secret", fp["HIVE_CDN_KEY"])
	}
	if fp["LOG_LEVEL"] != "=info" {
		t.Error("a variable nobody manages was lost")
	}
	if len(got.Env) != 4 {
		t.Errorf("env has %d entries, want 4 — a value was duplicated rather than replaced", len(got.Env))
	}
}

// A variable the staged side does not set must not survive from the other side.
// Validation rejects that config, so this is the belt to that braces: the driver
// removes what it is told it manages, whether or not a value replaces it.
func TestSideEnvDropsWhatTheStagedSideDoesNotSet(t *testing.T) {
	current := []*armappcontainers.Container{
		container("main", "reg.azurecr.io/site:old",
			literal("GATEWAY_URL", "https://gw---blue.example.com"),
			literal("LOG_LEVEL", "info"),
		),
	}

	got := findContainer(
		withSide(current, "main", "green", nil, []string{"GATEWAY_URL"}), "main")

	if _, ok := envFingerprint(got.Env, nil)["GATEWAY_URL"]; ok {
		t.Error("the other side's value survived into the staged revision")
	}
}

// Per-side values differ by side by definition, so they cannot be part of the
// comparison: a diff over them would report a change on every single run.
func TestSideEnvIsNotCompared(t *testing.T) {
	current := []*armappcontainers.Container{
		container("main", "reg.azurecr.io/gateway:old",
			literal("HIVE_CDN_ENDPOINT", "https://cdn/artifacts/v1/tst-blue"),
		),
	}
	sideEnv := []target.EnvVar{
		{Name: "HIVE_CDN_ENDPOINT", Value: refs.Value{
			Kind: refs.Literal, Literal: "https://cdn/artifacts/v1/tst-green"}},
	}
	managed := []string{"HIVE_CDN_ENDPOINT"}

	next := withSide(current, "main", "green", sideEnv, managed)

	added, changed, removed := diffContainers(current, next, "main", managed)
	if len(added)+len(changed)+len(removed) != 0 {
		t.Errorf("the side environment produced a diff: +%v ~%v -%v", added, changed, removed)
	}
}

// "Already deactivated" is the state the call was asking for. Treating it as a
// failure turned a clean abandon into "could not put the traffic back", which
// was untrue: the traffic went back and only a redundant call complained.
func TestAlreadyDeactivatedIsNotAFailure(t *testing.T) {
	body := `{"error":{"code":"RevisionAlreadyInRequestedState",` +
		`"message":"Revision app--0000027 is already deactivated!."}}`
	conflict := &azcore.ResponseError{
		ErrorCode:  "RevisionAlreadyInRequestedState",
		StatusCode: http.StatusConflict,
		RawResponse: &http.Response{
			StatusCode: http.StatusConflict,
			Body:       io.NopCloser(strings.NewReader(body)),
		},
	}

	if !alreadyInState(conflict) {
		t.Error("a 409 RevisionAlreadyInRequestedState should be read as success")
	}
	if !alreadyInState(fmt.Errorf("deactivating app--0000027: %w", conflict)) {
		t.Error("it should still be recognised once wrapped")
	}

	other := &azcore.ResponseError{
		ErrorCode:  "AuthorizationFailed",
		StatusCode: http.StatusForbidden,
	}
	if alreadyInState(other) {
		t.Error("any other ARM error is a real failure")
	}
	if alreadyInState(errors.New("dial tcp: no such host")) {
		t.Error("a plain error is a real failure")
	}
}

// What a settle leaves running is the cost of the feature, so it is worth
// pinning: one revision by default, and the previous one too when someone has
// said the cold start is the more expensive half.
func TestSettleKeep(t *testing.T) {
	change := func(keepWarm *bool) *target.Change {
		return &target.Change{
			Target: &config.Target{
				Name:     "site",
				Strategy: &config.Strategy{Type: config.StrategyBlueGreen, KeepWarm: keepWarm},
			},
			Sides: &target.Sides{
				Active: target.Side{Label: "blue", Revision: "site--rev-a"},
				Idle:   target.Side{Label: "green", Revision: "site--rev-b"},
			},
			Payload: &bgPayload{staged: "site--rev-b"},
		}
	}

	cases := []struct {
		name string
		ch   *target.Change
		want []string
	}{
		{
			// The default. The previous version keeps its label at 0% so the
			// rollback still has a name, but a version nobody is using does not
			// keep its replicas.
			name: "unset switches the previous side off",
			ch:   change(nil),
			want: []string{"site--rev-b"},
		},
		{
			name: "false switches the previous side off",
			ch:   change(to.Ptr(false)),
			want: []string{"site--rev-b"},
		},
		{
			name: "true leaves it running",
			ch:   change(to.Ptr(true)),
			want: []string{"site--rev-b", "site--rev-a"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := settleKeep(tc.ch)
			if len(got) != len(tc.want) {
				t.Fatalf("keep = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("keep = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Point has to start the revision it is about to hand traffic to, and this is
// how it knows which one that is.
func TestServingRevision(t *testing.T) {
	got := servingRevision([]*armappcontainers.TrafficWeight{
		tw("blue", "site--rev-a", 0),
		tw("green", "site--rev-b", 100),
	})
	if got != "site--rev-b" {
		t.Errorf("serving = %q, want site--rev-b", got)
	}

	// Nothing at 100% means nothing to start, and reading that as "the first
	// one" would start the side being rolled away from.
	if got := servingRevision([]*armappcontainers.TrafficWeight{
		tw("blue", "site--rev-a", 50),
		tw("green", "site--rev-b", 50),
	}); got != "" {
		t.Errorf("a split has no single serving revision, got %q", got)
	}
}

// A tidy has no release to ask which revision it just made, so it reads the
// traffic block instead. What it keeps is the same answer Settle gives.
func TestTidyKeep(t *testing.T) {
	block := []*armappcontainers.TrafficWeight{
		tw("blue", "site--rev-a", 0),
		tw("green", "site--rev-b", 100),
		tw("", "site--rev-old", 0),
	}

	got := tidyKeep(block, false)
	if len(got) != 1 || got[0] != "site--rev-b" {
		t.Errorf("keep = %v, want only the serving revision", got)
	}

	// keep_warm spares the other label, and only the other label: an entry with
	// no label is a leftover whichever way the knob is set.
	got = tidyKeep(block, true)
	if len(got) != 2 || got[0] != "site--rev-b" || got[1] != "site--rev-a" {
		t.Errorf("keep = %v, want the serving revision and the other label", got)
	}

	// A split is refused rather than guessed at. Switching off the wrong half
	// of one is not a cleanup, it is an outage.
	if got := tidyKeep([]*armappcontainers.TrafficWeight{
		tw("blue", "site--rev-a", 30),
		tw("green", "site--rev-b", 70),
	}, false); got != nil {
		t.Errorf("keep = %v, want nothing to act on", got)
	}
}

// bake_time is refused rather than ignored, and the refusal points at the
// setting that answers the same question here. Two spellings that both did
// something would make the pair mean two things at once; one that silently did
// nothing was worse, because a rollback window is exactly the thing nobody
// notices the absence of until they need it.
func TestBakeTimeIsRefusedOnContainerApps(t *testing.T) {
	want := &target.Desired{
		Service: "site",
		Target: &config.Target{
			Type: config.TypeContainerApp,
			Name: "evolve-prd-site",
			Strategy: &config.Strategy{
				Type:     config.StrategyBlueGreen,
				BakeTime: "10m",
			},
		},
	}

	_, err := (&Driver{}).planAppBlueGreen(context.Background(), want)
	if err == nil {
		t.Fatal("bake_time was accepted on container apps")
	}
	for _, want := range []string{"evolve-prd-site", "bake_time", "keep_warm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}
