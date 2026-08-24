package plan

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// A hook that registers an address writes it down for something else to read
// later, so what it wants is the address the service keeps — not the side it
// happened to be staged on. url is the short spelling of url_public.
func TestUrlInAHookIsThePublicAddress(t *testing.T) {
	for _, fn := range []string{"url", "url_public"} {
		dir := t.TempDir()
		seen := filepath.Join(dir, "url")

		d := newRolloutDriver()
		p, err := Build(context.Background(), load(t, header+`
strategy:
  type: blue-green
  smoke: [ "true" ]

services:
  site:
    version: new
    type: ecs
    cluster: platform
    after: [ "echo {{`+fn+` \"site\"}} > `+seen+`" ]
`), d, nil)
		if err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
		if err := Apply(context.Background(), p, Options{
			Driver: d, Hooks: &hooks.Runner{Out: io.Discard}, Out: &strings.Builder{},
		}); err != nil {
			t.Fatalf("%s: %v", fn, err)
		}

		raw, err := os.ReadFile(seen)
		if err != nil {
			t.Fatal(err)
		}
		// The side address is https://site---green.example, and publishing that
		// would pin whatever read it to a label that means something else from
		// the next release on.
		if got := strings.TrimSpace(string(raw)); got != "https://site.example" {
			t.Errorf("%s = %q", fn, got)
		}
	}
}

// The three addresses a release has, told apart by what re-points them.
func TestSmokeCanNameAllThreeAddresses(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "urls")

	d := newRolloutDriver()
	p, err := Build(context.Background(), load(t, header+`
strategy:
  type: blue-green
  smoke:
    - echo {{url_revision "site"}} {{url_stage "site"}} {{url_public "site"}} > `+seen+`

services:
  site:
    version: new
    type: ecs
    cluster: platform
`), d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), p, Options{
		Driver: d, Hooks: &hooks.Runner{Out: io.Discard}, Out: &strings.Builder{},
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://site--new-rev.example " +
		"https://site---green.example " +
		"https://site.example"
	if got := strings.TrimSpace(string(raw)); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

// url used to mean the staged side. Resolving it to the public address in a
// smoke block would keep rendering, keep passing, and start testing the version
// the release replaces — so there it is a refusal that names both spellings.
func TestBareUrlInSmokeIsRefused(t *testing.T) {
	d := newRolloutDriver()
	_, err := Build(context.Background(), load(t, header+`
strategy:
  type: blue-green
  smoke: [ "curl {{url \"site\"}}/healthz" ]

services:
  site:
    version: new
    type: ecs
    cluster: platform
`), d, nil)
	if err == nil {
		t.Fatal("want a plan error")
	}
	for _, want := range []string{`{{url_stage "site"}}`, `{{url_public "site"}}`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should offer %q, got %v", want, err)
		}
	}
	if len(d.took()) != 0 {
		t.Errorf("something was deployed before the smoke block was checked: %v", d.took())
	}
}

// url_stage in an after hook renders to an address that answers correctly at
// that moment and names the wrong revision from the next release on.
func TestStageAddressInAnAfterHookSaysWhereItBelongs(t *testing.T) {
	d := newRolloutDriver()
	_, err := Build(context.Background(), load(t, header+`
strategy:
  type: blue-green

services:
  site:
    version: new
    type: ecs
    cluster: platform
    after: [ "publish --url {{url_stage \"site\"}}" ]
`), d, nil)
	if err == nil {
		t.Fatal("want a plan error")
	}
	for _, want := range []string{"strategy.smoke", `{{url "site"}}`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q, got %v", want, err)
		}
	}
	if len(d.took()) != 0 {
		t.Errorf("something was deployed before the hook was checked: %v", d.took())
	}
}

// Only Container Apps gives a revision an address of its own. Falling back to
// the label's would answer a different question than the one that was asked, so
// the refusal names the function that does reach the same revision.
func TestRevisionAddressIsRefusedWhereThereIsNone(t *testing.T) {
	ch := &target.Change{
		Service:   "sync",
		Target:    &config.Target{Type: config.TypeCloudRun, Name: "sync"},
		PublicURL: "https://sync.example",
	}
	got := &target.Staged{
		Label:    "green",
		Revision: "sync-00002",
		URL:      "https://green---sync.example",
		// No RevisionURL: a Cloud Run revision is reachable through a tag and
		// not otherwise.
	}
	l := stagedLookup(
		[]bgTarget{{ch: ch, cp: &ServicePlan{Service: &config.Service{Name: "sync"}}}},
		map[*target.Change]*target.Staged{ch: got},
	)

	if _, err := l(urlRevision, "sync"); err == nil {
		t.Fatal("a revision with no address of its own should not resolve")
	} else if !strings.Contains(err.Error(), `{{url_stage "sync"}}`) {
		t.Errorf("the refusal should name what does reach it, got %v", err)
	}

	// The other two are there, so the refusal is about this address and not
	// about the target.
	if url, err := l(urlStage, "sync"); err != nil || url != got.URL {
		t.Errorf("url_stage = %q, %v", url, err)
	}
	if url, err := l(urlPublic, "sync"); err != nil || url != ch.PublicURL {
		t.Errorf("url_public = %q, %v", url, err)
	}
}

// A name the release does not deploy is refused while planning, for the same
// reason a smoke name is: after a staging phase that took minutes is a bad time
// to find out about a typo.
func TestHookUrlNamesAreCheckedWhilePlanning(t *testing.T) {
	d := newRolloutDriver()
	_, err := Build(context.Background(), load(t, header+`
services:
  site:
    version: new
    type: ecs
    cluster: platform
    after: [ "publish --url {{url \"sight\"}}" ]
`), d, nil)
	if err == nil {
		t.Fatal("want a plan error")
	}
	if !strings.Contains(err.Error(), "not deployed by this release") ||
		!strings.Contains(err.Error(), "site") {
		t.Errorf("the error should say what is available, got %v", err)
	}
	if len(d.took()) != 0 {
		t.Errorf("something was deployed before the hook was checked: %v", d.took())
	}
}

// A rider has no ingress, so there is no address to give a hook. An empty
// string would publish a registration pointing at nothing.
//
// Named beside a target that does have one, so this is the refusal for "no
// address" and not the one for a release that has none at all.
func TestHookUrlRefusesATargetWithNoAddress(t *testing.T) {
	_, err := Build(context.Background(), load(t, header+`
services:
  discover:
    version: new
    cluster: platform
    after: [ "publish --url {{url \"sync-events\"}}" ]
    targets:
      - { type: ecs, name: discover }
      - { type: lambda, name: sync-events, code: { bucket: b, key: k.zip } }
`), newRolloutDriver(), nil)
	if err == nil {
		t.Fatal("want a plan error")
	}
	if !strings.Contains(err.Error(), "no address of its own") {
		t.Errorf("the error should say the target has no address, got %v", err)
	}
}

// A service whose name could mean either of two addresses is refused rather
// than resolved to whichever was planned first.
func TestHookUrlRefusesAnAmbiguousServiceName(t *testing.T) {
	_, err := Build(context.Background(), load(t, header+`
services:
  edge:
    version: new
    cluster: platform
    after: [ "publish --url {{url \"edge\"}}" ]
    targets:
      - { type: ecs, name: edge-eu }
      - { type: ecs, name: edge-us }
`), newRolloutDriver(), nil)
	if err == nil {
		t.Fatal("want a plan error")
	}
	for _, want := range []string{"cannot mean either", "edge-eu", "edge-us"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q, got %v", want, err)
		}
	}
}
