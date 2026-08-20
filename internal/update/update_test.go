package update

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/gitlog"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// fakeDriver answers Artifacts from a table keyed by target name. Everything
// else on the interface is unreachable from Questions.
type fakeDriver struct {
	// available maps target name to the versions it has an artifact for. A
	// target that is absent from the map has nothing.
	available map[string][]string
	// unknown names targets whose artifacts cannot be listed.
	unknown map[string]bool
	// broken names targets whose lookup fails outright.
	broken map[string]bool
}

func (d *fakeDriver) Name() string                                     { return "fake" }
func (d *fakeDriver) Capabilities(config.TargetType) target.Capability { return target.Capability{} }
func (d *fakeDriver) Verify(context.Context) error                     { return nil }
func (d *fakeDriver) Resolver() refs.Resolver                          { return nil }
func (d *fakeDriver) Apply(context.Context, *target.Change) error      { return nil }
func (d *fakeDriver) Revert(context.Context, *target.Change) error     { return nil }
func (d *fakeDriver) Plan(context.Context, *target.Desired) (*target.Change, error) {
	return nil, nil
}

func (d *fakeDriver) Artifacts(
	_ context.Context, t *config.Target, versions []string,
) ([]string, error) {
	switch {
	case d.broken[t.Name]:
		return nil, fmt.Errorf("permission denied")
	case d.unknown[t.Name]:
		return nil, fmt.Errorf("nginx:1.27: %w", target.ErrArtifactsUnknown)
	}

	have := map[string]bool{}
	for _, v := range d.available[t.Name] {
		have[v] = true
	}
	return target.Present(versions, have), nil
}

// commits are newest first, the way git log reports them.
var commits = []gitlog.Commit{
	{Version: "ddd4444", Subject: "chore: bump deps"},
	{Version: "ccc3333", Subject: "feat: bulk index endpoint"},
	{Version: "bbb2222", Subject: "fix: retry on 429"},
	{Version: "aaa1111", Subject: "feat: first"},
}

func questionsFor(t *testing.T, body string, d *fakeDriver) []*Choice {
	t.Helper()

	e, path := open(t, body)
	f, err := config.Load(path, "acc")
	if err != nil {
		t.Fatal(err)
	}

	choices, err := Questions(context.Background(), f, d, e, commits)
	if err != nil {
		t.Fatal(err)
	}
	return choices
}

func versionsOf(c *Choice) []string {
	out := make([]string, len(c.Options))
	for i, o := range c.Options {
		out[i] = o.Version
	}
	return out
}

const azureHeader = `cloud:
  provider: azure
  subscription: "0000"
  resource_group: evolve-acc

`

// A service is one version across all of its targets, so a version is only
// offered when every one of them has it. Anything else would produce the
// half-deployed release the plan phase exists to prevent.
func TestOptionsAreWhatEveryTargetHas(t *testing.T) {
	choices := questionsFor(t, azureHeader+`services:
  discover:
    version: aaa1111
    targets:
      - { type: container-app,     name: discover }
      - { type: container-app-job, name: discover-products }
`, &fakeDriver{available: map[string][]string{
		"discover":          {"ddd4444", "ccc3333", "bbb2222", "aaa1111"},
		"discover-products": {"ccc3333", "aaa1111"},
	}})

	if len(choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(choices))
	}
	// Newest first, the order the commits came in — not the order a registry
	// happens to report its tags in.
	want := []string{"ccc3333", "aaa1111"}
	if got := versionsOf(choices[0]); !slices.Equal(got, want) {
		t.Errorf("options = %v, want %v", got, want)
	}
	if choices[0].Note != "" {
		t.Errorf("Note = %q, want none", choices[0].Note)
	}
}

func TestOptionsCarryTheirCommitSubject(t *testing.T) {
	choices := questionsFor(t, azureHeader+`services:
  discover:
    version: aaa1111
    type: container-app
`, &fakeDriver{available: map[string][]string{"discover": {"bbb2222"}}})

	if got := choices[0].Options[0].Subject; got != "fix: retry on 429" {
		t.Errorf("subject = %q, want the commit's subject", got)
	}
	if got := choices[0].Current; got != "aaa1111" {
		t.Errorf("Current = %q, want aaa1111", got)
	}
}

// An image in a registry the tool cannot list is still deployable. Offering
// nothing would be wrong; offering everything with a note is the honest answer.
func TestUnlistableTargetOffersEveryCommit(t *testing.T) {
	choices := questionsFor(t, azureHeader+`services:
  discover:
    version: aaa1111
    type: container-app
`, &fakeDriver{unknown: map[string]bool{"discover": true}})

	if got := len(choices[0].Options); got != len(commits) {
		t.Errorf("got %d options, want all %d commits", got, len(commits))
	}
	if !strings.Contains(choices[0].Note, "could not check") {
		t.Errorf("Note = %q, want it to say the list was not checked", choices[0].Note)
	}
}

// One target that cannot be listed must not widen the list back out: the
// targets that could be checked still constrain it, and the note says which
// ones were not.
func TestPartlyUnlistableServiceStaysConstrained(t *testing.T) {
	choices := questionsFor(t, azureHeader+`services:
  discover:
    version: aaa1111
    targets:
      - { type: container-app,     name: discover }
      - { type: container-app-job, name: discover-products }
`, &fakeDriver{
		available: map[string][]string{"discover": {"ccc3333"}},
		unknown:   map[string]bool{"discover-products": true},
	})

	want := []string{"ccc3333"}
	if got := versionsOf(choices[0]); !slices.Equal(got, want) {
		t.Errorf("options = %v, want %v", got, want)
	}
	if !strings.Contains(choices[0].Note, "discover-products") {
		t.Errorf("Note = %q, want it to name the target that was not checked",
			choices[0].Note)
	}
}

func TestServiceWithNothingBuiltSaysSo(t *testing.T) {
	choices := questionsFor(t, azureHeader+`services:
  discover:
    version: aaa1111
    type: container-app
`, &fakeDriver{})

	if len(choices[0].Options) != 0 {
		t.Fatalf("options = %v, want none", versionsOf(choices[0]))
	}
	if !strings.Contains(choices[0].Note, "never built") &&
		!strings.Contains(choices[0].Note, "was ever built") {
		t.Errorf("Note = %q, want it to explain the empty list", choices[0].Note)
	}
}

// The questions come in file order, so the answers land in the same order as
// the diff someone is about to review.
func TestChoicesFollowTheFile(t *testing.T) {
	choices := questionsFor(t, azureHeader+`services:
  site:
    version: aaa1111
    type: container-app
  discover:
    version: aaa1111
    type: container-app
  api:
    version: aaa1111
    type: container-app
`, &fakeDriver{available: map[string][]string{
		"site":     {"bbb2222"},
		"discover": {"bbb2222"},
		"api":      {"bbb2222"},
	}})

	var got []string
	for _, c := range choices {
		got = append(got, c.Service)
	}
	want := []string{"site", "discover", "api"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v (the order they appear in the file)", got, want)
	}
}

// A lookup that fails for a real reason — no permission on the registry — is
// not a short list, and it must stop the run before any question is asked.
func TestFailedLookupsAreReportedTogether(t *testing.T) {
	e, path := open(t, azureHeader+`services:
  site:
    version: aaa1111
    type: container-app
  discover:
    version: aaa1111
    type: container-app
`)
	f, err := config.Load(path, "acc")
	if err != nil {
		t.Fatal(err)
	}

	_, err = Questions(context.Background(), f, &fakeDriver{
		broken: map[string]bool{"site": true, "discover": true},
	}, e, commits)
	if err == nil {
		t.Fatal("want an error when the registry cannot be read")
	}
	for _, name := range []string{"site", "discover"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error = %q, want it to mention %s", err, name)
		}
	}
	if errors.Is(err, target.ErrArtifactsUnknown) {
		t.Error("a permission failure must not be reported as an unlistable registry")
	}
}
