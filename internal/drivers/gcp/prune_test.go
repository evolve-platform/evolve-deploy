package gcp

import (
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// The protected set is the whole safety of this command, and the entry a
// hand-written script misses is the untagged one the note names.
func TestProtectedRevisions(t *testing.T) {
	svc := &runpb.Service{
		Traffic: []*runpb.TrafficTarget{
			tt("green", "site-00012", 100),
		},
		Annotations: map[string]string{
			rollbackAnnotation: "blue=site-00001",
		},
		LatestReadyRevision:   "projects/p/locations/l/services/site/revisions/site-00012",
		LatestCreatedRevision: "projects/p/locations/l/services/site/revisions/site-00013",
	}

	got := protectedRevisions(svc)

	for _, revision := range []string{"site-00012", "site-00001", "site-00013"} {
		if got[revision] == "" {
			t.Errorf("%s is not protected, got %v", revision, got)
		}
	}
	if got["site-00011"] != "" {
		t.Errorf("site-00011 should not be protected, got %q", got["site-00011"])
	}
	// The most concrete reason is the one worth printing.
	if got["site-00012"] != "serving traffic" {
		t.Errorf("site-00012 kept for %q", got["site-00012"])
	}
	if got["site-00001"] != "recorded as the blue side to roll back to" {
		t.Errorf("site-00001 kept for %q", got["site-00001"])
	}
}

// A LATEST entry names no revision, so the revision it resolves to has to be
// asked for separately or a prune would delete what is serving.
func TestProtectedRevisionsCoversALatestEntry(t *testing.T) {
	svc := &runpb.Service{
		Traffic:             []*runpb.TrafficTarget{ttLatest("blue", 100)},
		LatestReadyRevision: "projects/p/locations/l/services/site/revisions/site-00007",
	}
	if got := protectedRevisions(svc); got["site-00007"] == "" {
		t.Errorf("the revision LATEST resolves to is not protected, got %v", got)
	}
}

func TestPlanPrune(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	daysAgo := func(d int) time.Time { return now.AddDate(0, 0, -d) }

	looked := planPrune([]prunableRevision{
		{name: "site-00001", created: daysAgo(400)}, // old, and the rollback target
		{name: "site-00002", created: daysAgo(400)}, // old, nothing needs it
		{name: "site-00003", created: daysAgo(91)},  // just past the window
		{name: "site-00004", created: daysAgo(89)},  // just inside it
		{name: "site-00012", created: daysAgo(200)}, // old, but serving
		{name: "site-00013"},                        // no creation time at all
	}, map[string]string{
		"site-00001": "recorded as the blue side to roll back to",
		"site-00012": "serving traffic",
	}, now)

	keep := map[string]string{}
	for _, p := range looked {
		keep[p.Revision] = p.Keep
	}

	// Age never overrides the protected set: a revision that is serving, or is
	// the way back, is kept however old it is.
	for _, revision := range []string{"site-00001", "site-00012"} {
		if keep[revision] == "" {
			t.Errorf("%s was removed and must never be", revision)
		}
	}
	// Nor does the protected set have to explain a revision that is simply young.
	if keep["site-00004"] == "" {
		t.Error("a revision inside the retention window was removed")
	}
	// And what is both old and unreferenced is exactly what this is for.
	for _, revision := range []string{"site-00002", "site-00003"} {
		if keep[revision] != "" {
			t.Errorf("%s was kept: %s", revision, keep[revision])
		}
	}
	// Not knowing how old something is reads as a reason to keep it.
	if keep["site-00013"] == "" {
		t.Error("a revision with no creation time was removed")
	}

	if len(looked) != 6 {
		t.Errorf("looked at %d revisions, want all 6 reported back", len(looked))
	}
}

// The boundary is worth pinning down, because the window is the only thing
// standing between a busy service's recent history and a delete.
func TestPlanPruneRetentionBoundary(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	looked := planPrune([]prunableRevision{
		{name: "inside", created: now.Add(-target.PruneRetention + time.Hour)},
		{name: "outside", created: now.Add(-target.PruneRetention - time.Hour)},
	}, nil, now)

	for _, p := range looked {
		switch p.Revision {
		case "inside":
			if p.Removed() {
				t.Error("a revision one hour inside the window was removed")
			}
		case "outside":
			if !p.Removed() {
				t.Errorf("a revision one hour past the window was kept: %s", p.Keep)
			}
		}
	}
}
