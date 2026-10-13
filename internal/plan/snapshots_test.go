package plan

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

type snapshotResolver struct {
	*fakeResolver
	mu      sync.Mutex
	calls   map[refs.Kind]int
	version string
}

func (r *snapshotResolver) Pin(_ context.Context, v refs.Value) (refs.Value, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[v.Kind]++
	if v.Kind == refs.ParameterVersion {
		return refs.Value{Kind: refs.Literal, Literal: r.version, Raw: v.Raw}, nil
	}
	v.Name += "/versions/7"
	return v, nil
}

type snapshotDriver struct {
	*fakeDriver
	resolver *snapshotResolver
}

func (d *snapshotDriver) Resolver() refs.Resolver { return d.resolver }

func TestSnapshotIsSharedAcrossTargetsAndFreshForEachPlan(t *testing.T) {
	base := newFakeDriver()
	base.caps[config.TypeECS] = target.Capability{NativeSecret: true}
	d := &snapshotDriver{fakeDriver: base, resolver: &snapshotResolver{
		fakeResolver: base.res, calls: map[refs.Kind]int{}, version: "config-a",
	}}
	f := load(t, header+`
refs:
  resolve: deny
services:
  purchase:
    version: image-a
    cluster: app
    env:
      PARAMETER_MANAGER_VERSION: ${parameter-version:purchase}
      TOKEN: ${secret:token}
    targets:
      - {type: ecs, name: purchase}
      - {type: ecs, name: purchase-events}
      - {type: ecs, name: purchase-job}
  sibling:
    version: image-a
    cluster: app
    env:
      PARAMETER_MANAGER_VERSION: ${parameter-version:purchase}
    targets:
      - {type: ecs, name: sibling}
`)
	if _, err := Build(context.Background(), f, d, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"purchase", "purchase-events", "purchase-job", "sibling"} {
		if got := envOf(t, base, name)["PARAMETER_MANAGER_VERSION"].Literal; got != "config-a" {
			t.Fatalf("%s got %q", name, got)
		}
	}
	if got := envOf(t, base, "purchase")["TOKEN"].Name; got != "token/versions/7" {
		t.Fatal(got)
	}
	if d.resolver.calls[refs.Secret] != 1 || d.resolver.calls[refs.ParameterVersion] != 1 {
		t.Fatalf("resolved more than once: %v", d.resolver.calls)
	}
	old := envOf(t, base, "purchase")
	d.resolver.version = "config-b"
	if _, err := Build(context.Background(), f, d, nil); err != nil {
		t.Fatal(err)
	}
	if got := envOf(t, base, "purchase")["PARAMETER_MANAGER_VERSION"].Literal; got != "config-b" {
		t.Fatal(got)
	}
	if old["PARAMETER_MANAGER_VERSION"].Literal != "config-a" {
		t.Fatal("existing revision changed")
	}
	if len(base.res.reads) != 0 {
		t.Fatalf("read payloads: %v", base.res.reads)
	}
}

func TestParameterVersionIsRefusedOnUnsupportedCloud(t *testing.T) {
	f := load(t, header+`
services:
  purchase:
    version: a
    type: ecs
    cluster: app
    env:
      CONFIG_VERSION: ${parameter-version:purchase}
`)
	_, err := Build(context.Background(), f, newFakeDriver(), nil)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("got %v", err)
	}
}
