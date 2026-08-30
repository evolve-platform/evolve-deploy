package gcp

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

func tt(tag, revision string, percent int32) *runpb.TrafficTarget {
	return &runpb.TrafficTarget{
		Type:     runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION,
		Tag:      tag,
		Revision: revision,
		Percent:  percent,
	}
}

func ttLatest(tag string, percent int32) *runpb.TrafficTarget {
	return &runpb.TrafficTarget{
		Type:    runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST,
		Tag:     tag,
		Percent: percent,
	}
}

var labels = []string{"blue", "green"}

// The refusal table, and it is the same rule as on Container Apps: active is
// the tag with 100% of the traffic, and every other shape is a refusal rather
// than a guess. Only the vocabulary differs — tag for label, percent for
// weight.
func TestReadSides(t *testing.T) {
	cases := []struct {
		name          string
		traffic       []*runpb.TrafficTarget
		wantActive    string
		wantIdle      string
		wantIdleRev   string
		wantPinNeeded bool
		wantErr       string
	}{{
		name:        "one side serving, the other behind it",
		traffic:     []*runpb.TrafficTarget{tt("blue", "site-00001", 100), tt("green", "site-00002", 0)},
		wantActive:  "blue",
		wantIdle:    "green",
		wantIdleRev: "site-00002",
	}, {
		// A service deployed this way for the first time has one side and no
		// other, and that is not an error — it is where everyone starts.
		name:       "the idle tag does not exist yet",
		traffic:    []*runpb.TrafficTarget{tt("blue", "site-00001", 100)},
		wantActive: "blue",
		wantIdle:   "green",
	}, {
		// "Whatever is newest" is a rule, not a reference, and it has to become
		// a fact before a revision is created — otherwise the staged one takes
		// all the traffic the moment it exists.
		name:          "the serving tag is on latest",
		traffic:       []*runpb.TrafficTarget{ttLatest("blue", 100), tt("green", "site-00002", 0)},
		wantActive:    "blue",
		wantIdle:      "green",
		wantIdleRev:   "site-00002",
		wantPinNeeded: true,
	}, {
		name:    "a split has no active side",
		traffic: []*runpb.TrafficTarget{tt("blue", "site-00001", 60), tt("green", "site-00002", 40)},
		wantErr: "traffic is split",
	}, {
		// Cloud Run's own default. There is no tag to fall back to, so this is
		// a service nobody has set up for this yet rather than a broken one.
		name:    "an empty block is the default of everything to newest",
		traffic: nil,
		wantErr: "the traffic block is empty",
	}, {
		name:    "everything on an untagged entry",
		traffic: []*runpb.TrafficTarget{tt("", "site-00001", 100)},
		wantErr: "no tag",
	}, {
		name:    "a tag the config does not know",
		traffic: []*runpb.TrafficTarget{tt("staging", "site-00001", 100)},
		wantErr: "not one of blue or green",
	}, {
		// What a Terraform bootstrap actually produces: the 100% entry and the
		// tag written as two blocks, both following whatever is newest. Cloud
		// Run serves that as one tagged entry at 100%, so blue is the active
		// side even though the entry carrying the percentage has no tag.
		name:          "the tag rides in an entry of its own",
		traffic:       []*runpb.TrafficTarget{ttLatest("", 100), ttLatest("blue", 0)},
		wantActive:    "blue",
		wantIdle:      "green",
		wantPinNeeded: true,
	}, {
		name:       "the tag rides beside a pinned entry",
		traffic:    []*runpb.TrafficTarget{tt("", "site-00001", 100), tt("blue", "site-00001", 0)},
		wantActive: "blue",
		wantIdle:   "green",
	}, {
		// Both sides claiming the serving revision leaves nothing to deploy
		// away from, so this stays a refusal rather than a coin toss.
		name:    "both tags alias the serving revision",
		traffic: []*runpb.TrafficTarget{ttLatest("", 100), ttLatest("blue", 0), ttLatest("green", 0)},
		wantErr: "no tag",
	}, {
		// A tag pinned to a revision is not the untagged rule that happens to
		// point at it today, so this is not an alias.
		name:    "a pinned tag does not alias a latest entry",
		traffic: []*runpb.TrafficTarget{ttLatest("", 100), tt("blue", "site-00001", 0)},
		wantErr: "no tag",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readSides(c.traffic, labels)

			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("no error, want one mentioning %q", c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Active.Label != c.wantActive || got.Idle.Label != c.wantIdle {
				t.Errorf("active/idle = %s/%s, want %s/%s",
					got.Active.Label, got.Idle.Label, c.wantActive, c.wantIdle)
			}
			if got.Idle.Revision != c.wantIdleRev {
				t.Errorf("idle revision = %q, want %q", got.Idle.Revision, c.wantIdleRev)
			}
			if got.PinNeeded != c.wantPinNeeded {
				t.Errorf("pinNeeded = %v, want %v", got.PinNeeded, c.wantPinNeeded)
			}
		})
	}
}

// A tag cannot point at nothing, so the first release writes one entry rather
// than one entry and an empty one.
func TestWeightsLeavesOutASideWithNoRevision(t *testing.T) {
	got := weights(
		trafficEntry{target.Side{Label: "blue", Revision: "site-00001"}, weightAll},
		trafficEntry{target.Side{Label: "green"}, weightNone},
	)
	if len(got) != 1 || got[0].GetTag() != "blue" {
		t.Fatalf("weights = %v", got)
	}
}

// Every entry this tool writes names a revision. LATEST would hand the next
// revision everything the moment it is created, which is the one thing staging
// has to prevent.
func TestWeightsAlwaysPin(t *testing.T) {
	for _, w := range weights(
		trafficEntry{target.Side{Label: "blue", Revision: "site-00001"}, weightAll},
		trafficEntry{target.Side{Label: "green", Revision: "site-00002"}, weightNone},
	) {
		if w.GetType() != runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION {
			t.Errorf("%s is %s, want an explicit revision", w.GetTag(), w.GetType())
		}
	}
}

func TestPointTraffic(t *testing.T) {
	got, aside, err := pointTraffic([]*runpb.TrafficTarget{
		ttLatest("green", 100),
		tt("blue", "site-00001", 0),
	}, nil, "blue")
	if err != nil {
		t.Fatal(err)
	}

	// One entry on the way out. Green keeps no tag, because a tag is an address
	// and an address is what stops a revision being retired.
	if len(got) != 1 || got[0].GetTag() != "blue" || got[0].GetPercent() != weightAll {
		t.Fatalf("got %s", describeTraffic(got))
	}
	// Pinned on the way out: "whatever is newest" cannot be a resting state for
	// either side.
	if got[0].GetType() != runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION {
		t.Errorf("blue is still %s", got[0].GetType())
	}
	// And the side that lost its tag is handed back, so the caller can record it
	// before it becomes unfindable.
	if aside.Label != "green" {
		t.Errorf("aside = %+v, want green", aside)
	}

	if _, _, err := pointTraffic(
		[]*runpb.TrafficTarget{tt("blue", "site-00001", 100)}, nil, "green",
	); err == nil {
		t.Error("pointing at a side nothing names should fail")
	}
}

// The rollback path: after a switch the idle side has no tag at all, so the only
// thing that knows where blue went is the note on the service.
func TestPointTrafficUsesTheRecordedSide(t *testing.T) {
	got, aside, err := pointTraffic(
		[]*runpb.TrafficTarget{tt("green", "site-00002", 100)},
		map[string]string{rollbackAnnotation: "blue=site-00001"},
		"blue",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %s", describeTraffic(got))
	}
	if got[0].GetTag() != "blue" || got[0].GetRevision() != "site-00001" {
		t.Errorf("got tag %q revision %q", got[0].GetTag(), got[0].GetRevision())
	}
	if got[0].GetPercent() != weightAll {
		t.Errorf("blue = %d%%, want all of it", got[0].GetPercent())
	}
	// Rolling back makes green the side to come back to, and nothing else will
	// record that.
	if aside.Label != "green" || aside.Revision != "site-00002" {
		t.Errorf("aside = %+v, want green on site-00002", aside)
	}
}

// A note for the other side is not an answer for this one.
func TestPointTrafficWillNotBorrowTheOtherSidesNote(t *testing.T) {
	_, _, err := pointTraffic(
		[]*runpb.TrafficTarget{tt("green", "site-00002", 100)},
		map[string]string{rollbackAnnotation: "green=site-00002"},
		"blue",
	)
	if err == nil || !strings.Contains(err.Error(), "nothing names a revision") {
		t.Errorf("error = %v", err)
	}
}

// The Cloud Run cleanup is the traffic block itself: there is nothing to switch
// off, but a stray tag is a URL that answers, pointing at a version nobody
// chose.
func TestTidyTraffic(t *testing.T) {
	next, dropped := tidyTraffic([]*runpb.TrafficTarget{
		tt("blue", "site-00001", 0),
		tt("green", "site-00002", 100),
		tt("leftover", "site-00000", 0),
	}, labels)
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if len(next) != 2 {
		t.Fatalf("next = %v, want the two sides", next)
	}

	// Nothing to do is not the same as something to write.
	if _, dropped := tidyTraffic([]*runpb.TrafficTarget{
		tt("blue", "site-00001", 0),
		tt("green", "site-00002", 100),
	}, labels); dropped != 0 {
		t.Errorf("dropped = %d on a block that was already tidy", dropped)
	}

	// A split is refused rather than guessed at.
	if next, _ := tidyTraffic([]*runpb.TrafficTarget{
		tt("blue", "site-00001", 50),
		tt("green", "site-00002", 50),
	}, labels); next != nil {
		t.Error("a split has no single serving entry to tidy around")
	}
}

// Cloud Run returns a revision as a bare name in one place and as a resource
// path in another. A traffic block is written with the bare name.
func TestShortRevision(t *testing.T) {
	const want = "site-00007-abc"
	if got := shortRevision(
		"projects/p/locations/europe-west4/services/site/revisions/" + want); got != want {
		t.Errorf("short = %q", got)
	}
	if got := shortRevision(want); got != want {
		t.Errorf("a bare name should come back unchanged, got %q", got)
	}
}

// The tagged address comes from the API rather than being assembled, which is
// the one place Cloud Run is kinder than Container Apps.
func TestTaggedURL(t *testing.T) {
	statuses := []*runpb.TrafficTargetStatus{
		{Tag: "blue", Uri: "https://blue---site-abc.a.run.app"},
		{Tag: "green", Uri: "https://green---site-abc.a.run.app"},
	}
	if got := taggedURL(statuses, "green"); got != "https://green---site-abc.a.run.app" {
		t.Errorf("url = %q", got)
	}
	if got := taggedURL(statuses, "purple"); got != "" {
		t.Errorf("an unknown tag has no address, got %q", got)
	}
}

// The side is written into the container and then excluded from the diff. Both
// halves matter: without the write a service cannot address its own downstream,
// and without the exclusion every run would report a change and deploy forever.
func TestSideIsWrittenButNotCompared(t *testing.T) {
	current := template("", &runpb.Container{
		Name:  "app",
		Image: "eu.gcr.io/p/site:v1",
		Env: []*runpb.EnvVar{
			literal(target.SideEnvVar, "blue"),
			literal("KEEP", "yes"),
		},
	})

	next := withSide(current, "app", "green", nil, nil)

	env := envOf(next, "app")
	if env[target.SideEnvVar] != "=green" {
		t.Errorf("the side was not written: %v", env)
	}
	if env["KEEP"] != "=yes" {
		t.Errorf("an unrelated variable was lost: %v", env)
	}

	added, changed, removed := diffEnv(current.GetContainers(), next.GetContainers(), "app", nil)
	if len(added)+len(changed)+len(removed) != 0 {
		t.Errorf("the side produced a diff: +%v ~%v -%v", added, changed, removed)
	}
}

// The staged containers are copied from the *other* side's revision, so a
// variable this side does not set would arrive carrying the other side's value.
func TestSideEnvReplacesTheOtherSidesValues(t *testing.T) {
	current := template("", &runpb.Container{
		Name:  "app",
		Image: "eu.gcr.io/p/site:v1",
		Env:   []*runpb.EnvVar{literal("ROUTER_URL", "https://blue.example")},
	})

	managed := []string{"ROUTER_URL"}
	next := withSide(current, "app", "green", []target.EnvVar{
		{Name: "ROUTER_URL", Value: refs.Value{Kind: refs.Literal, Literal: "https://green.example"}},
	}, managed)

	if got := envOf(next, "app")["ROUTER_URL"]; got != "=https://green.example" {
		t.Errorf("ROUTER_URL = %q, want the green value", got)
	}

	// And it is invisible to the diff, because a value that differs by side by
	// definition is not a change anyone made.
	added, changed, removed := diffEnv(current.GetContainers(), next.GetContainers(), "app", managed)
	if len(added)+len(changed)+len(removed) != 0 {
		t.Errorf("the side environment produced a diff: +%v ~%v -%v", added, changed, removed)
	}
}

// Sidecars are Terraform's. A collector next to the app keeps its own image and
// does not get the side written into it.
func TestWithSideLeavesSidecarsAlone(t *testing.T) {
	current := template("",
		&runpb.Container{Name: "app", Image: "eu.gcr.io/p/site:v1"},
		&runpb.Container{Name: "collector", Image: "otel/collector:1"},
	)

	next := withSide(current, "app", "green", nil, nil)

	if _, ok := envOf(next, "collector")[target.SideEnvVar]; ok {
		t.Error("the sidecar was told which side it is on")
	}
	if envOf(next, "app")[target.SideEnvVar] != "=green" {
		t.Error("the application container was not")
	}
}

// envOf reads the environment as written. Deliberately not envFingerprint,
// which hides exactly the variables these tests are about.
func envOf(tmpl *runpb.RevisionTemplate, container string) map[string]string {
	out := map[string]string{}
	for _, e := range findContainer(tmpl.GetContainers(), container).GetEnv() {
		if src := e.GetValueSource().GetSecretKeyRef(); src != nil {
			out[e.GetName()] = "->" + src.GetSecret()
			continue
		}
		out[e.GetName()] = "=" + e.GetValue()
	}
	return out
}

// bake_time is refused rather than ignored. It is the window before ECS
// terminates the old side, and the setting parsing and validating cleanly on a
// cloud that never reads it is how a rollback window someone wrote down turns
// into one they do not have. The refusal is above every API call, so a zero
// Driver reaches it.
func TestBakeTimeIsRefusedOnCloudRun(t *testing.T) {
	want := &target.Desired{
		Service: "site",
		Target: &config.Target{
			Type: config.TypeCloudRun,
			Name: "site",
			Strategy: &config.Strategy{
				Type:     config.StrategyBlueGreen,
				BakeTime: "10m",
			},
		},
	}

	_, err := (&Driver{}).planServiceBlueGreen(context.Background(), want)
	if err == nil {
		t.Fatal("bake_time was accepted on cloud run")
	}
	for _, want := range []string{"site", "bake_time", "cannot be honoured"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// The version has to survive a round trip through a Cloud Run revision, which
// keeps the image as a digest and so cannot carry it. Staging writes the note
// and reading a side is what depends on it.
func TestVersionSurvivesARevision(t *testing.T) {
	current := &runpb.RevisionTemplate{
		Containers: []*runpb.Container{{
			Name:  "app",
			Image: "europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase@sha256:17df9cfe",
		}},
	}

	next, from, err := nextTemplate(current, "app", "27ec167", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The digest the tag resolved to is not the version it resolved from, so
	// there is nothing to report as the previous release here.
	if from != "" {
		t.Errorf("from = %q, want empty for a digest-pinned image", from)
	}
	if got := next.GetAnnotations()[versionAnnotation]; got != "27ec167" {
		t.Errorf("annotation = %q, want 27ec167", got)
	}
	if got := next.GetContainers()[0].GetImage(); !strings.HasSuffix(got, "/purchase:27ec167") {
		t.Errorf("image = %q, want the digest replaced by the tag", got)
	}

	// What Cloud Run gives back: the annotation as written, the image resolved
	// to a digest of its own.
	served := &runpb.Revision{
		Annotations: next.GetAnnotations(),
		Containers: []*runpb.Container{{
			Name:  "app",
			Image: "europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase@sha256:bdfeeb04",
		}},
	}
	if got := revisionVersion(served, "app"); got != "27ec167" {
		t.Errorf("revisionVersion = %q, want 27ec167", got)
	}
}

// A revision Terraform created, or one written before the note existed, has no
// annotation — and there the image may still carry a tag worth reading.
func TestRevisionVersionFallsBackToTheTag(t *testing.T) {
	rev := &runpb.Revision{
		Containers: []*runpb.Container{{
			Name:  "app",
			Image: "europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase:8618aae",
		}},
	}
	if got := revisionVersion(rev, "app"); got != "8618aae" {
		t.Errorf("revisionVersion = %q, want 8618aae", got)
	}

	// Neither an annotation nor a tag is unknown, not a digest hex.
	rev.Containers[0].Image = "europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase@sha256:bdfeeb04"
	if got := revisionVersion(rev, "app"); got != "" {
		t.Errorf("revisionVersion = %q, want empty", got)
	}
}

// Once a switch has dropped the idle tag, the note on the service is the only
// thing that knows where the other side went.
func TestResolveIdle(t *testing.T) {
	cases := []struct {
		name        string
		sides       *target.Sides
		note        string
		wantIdleRev string
	}{{
		name:        "the note names the idle side",
		sides:       &target.Sides{Active: target.Side{Label: "green", Revision: "site-00002"}, Idle: target.Side{Label: "blue"}},
		note:        "blue=site-00001",
		wantIdleRev: "site-00001",
	}, {
		// The traffic block is the better source and does not need the note. A
		// service deployed before this existed still has both tags.
		name:        "the block already answered",
		sides:       &target.Sides{Active: target.Side{Label: "green", Revision: "site-00002"}, Idle: target.Side{Label: "blue", Revision: "site-00001"}},
		note:        "blue=site-00009",
		wantIdleRev: "site-00001",
	}, {
		// A switch that died between its two writes leaves this. Believing it
		// would make one revision both sides at once.
		name:        "the note names the serving revision",
		sides:       &target.Sides{Active: target.Side{Label: "blue", Revision: "site-00001"}, Idle: target.Side{Label: "green"}},
		note:        "green=site-00001",
		wantIdleRev: "",
	}, {
		name:        "the note is for the other side",
		sides:       &target.Sides{Active: target.Side{Label: "green", Revision: "site-00002"}, Idle: target.Side{Label: "blue"}},
		note:        "green=site-00002",
		wantIdleRev: "",
	}, {
		name:        "no note at all",
		sides:       &target.Sides{Active: target.Side{Label: "green", Revision: "site-00002"}, Idle: target.Side{Label: "blue"}},
		note:        "",
		wantIdleRev: "",
	}, {
		name:        "a note nobody can parse",
		sides:       &target.Sides{Active: target.Side{Label: "green", Revision: "site-00002"}, Idle: target.Side{Label: "blue"}},
		note:        "blue=",
		wantIdleRev: "",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resolveIdle(c.sides, map[string]string{rollbackAnnotation: c.note})
			if c.sides.Idle.Revision != c.wantIdleRev {
				t.Errorf("idle revision = %q, want %q", c.sides.Idle.Revision, c.wantIdleRev)
			}
		})
	}
}

// The note has to survive a round trip, because a label on its own does not say
// which revision it meant — it alternates every release.
func TestRollbackNoteRoundTrip(t *testing.T) {
	note := formatRollback(target.Side{Label: "blue", Revision: "site-00001"})
	label, revision, ok := parseRollback(map[string]string{rollbackAnnotation: note})
	if !ok || label != "blue" || revision != "site-00001" {
		t.Errorf("parseRollback(%q) = (%q, %q, %v)", note, label, revision, ok)
	}
}
