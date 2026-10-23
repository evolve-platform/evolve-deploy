package azure

import (
	"context"
	"net/http"
	"strings"
	"testing"

	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3/fake"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// serving builds a driver whose app read always answers with this
// configuration and one application container on the given image.
func serving(t *testing.T, cfg *armappcontainers.Configuration, img string) *Driver {
	t.Helper()
	factory := fake.ServerFactory{
		ContainerAppsServer: fake.ContainerAppsServer{
			Get: func(
				_ context.Context, _, name string,
				_ *armappcontainers.ContainerAppsClientGetOptions,
			) (azfake.Responder[armappcontainers.ContainerAppsClientGetResponse], azfake.ErrorResponder) {
				var resp azfake.Responder[armappcontainers.ContainerAppsClientGetResponse]
				resp.SetResponse(http.StatusOK, armappcontainers.ContainerAppsClientGetResponse{
					ContainerApp: armappcontainers.ContainerApp{
						Name: to.Ptr(name),
						Properties: &armappcontainers.ContainerAppProperties{
							Configuration: cfg,
							Template: &armappcontainers.Template{
								Containers: []*armappcontainers.Container{container(appContainer, img)},
							},
						},
					},
				}, nil)
				return resp, azfake.ErrorResponder{}
			},
		},
	}
	return fakeDriver(t, &factory)
}

func workerTarget(strategy config.StrategyType) *config.Target {
	return &config.Target{
		Type:     config.TypeContainerApp,
		Name:     "evolve-tst-purchase-events",
		Ingress:  to.Ptr(false),
		Strategy: &config.Strategy{Type: strategy, Labels: config.DefaultLabels},
	}
}

func single() *armappcontainers.Configuration {
	return &armappcontainers.Configuration{
		ActiveRevisionsMode: to.Ptr(armappcontainers.ActiveRevisionsModeSingle),
	}
}

// A worker carries no traffic, so it is never a side of its own: on Container
// Apps that is the whole of what makes the plan ride it along at the switch.
func TestAWorkerIsNotRoutable(t *testing.T) {
	d := &Driver{}
	if d.Routable(workerTarget(config.StrategyBlueGreen)) {
		t.Error("a container app declared `ingress: false` is routable")
	}
	if d.Pointable(workerTarget(config.StrategyBlueGreen)) {
		t.Error("a container app declared `ingress: false` is pointable")
	}
	if !d.Routable(&config.Target{Type: config.TypeContainerApp}) {
		t.Error("a container app without the field stopped being routable")
	}
}

// In a blue-green service a worker is planned the direct way: no sides, no label
// to stage onto, just the template it will be patched to at the switch.
func TestAWorkerInABlueGreenServiceIsPlannedDirectly(t *testing.T) {
	d := serving(t, single(), "acr.io/purchase:old1234")

	ch, err := d.Plan(context.Background(), &target.Desired{
		Service: "purchase",
		Version: "new5678",
		Target:  workerTarget(config.StrategyBlueGreen),
	})
	if err != nil {
		t.Fatalf("planning a worker: %v", err)
	}
	if ch == nil {
		t.Fatal("expected a change")
	}
	if ch.Sides != nil {
		t.Errorf("a worker was given sides: %+v", ch.Sides)
	}
	if _, ok := ch.Payload.(*appPayload); !ok {
		t.Errorf("payload is %T, want the direct path's", ch.Payload)
	}
	if ch.FromVersion != "old1234" || ch.ToVersion != "new5678" {
		t.Errorf("versions = %s -> %s", ch.FromVersion, ch.ToVersion)
	}
}

// An unset revision mode reads back from an app Terraform created without
// saying, and it is Single: refusing it would refuse the default.
func TestAWorkerAcceptsAnUnsetRevisionMode(t *testing.T) {
	d := serving(t, &armappcontainers.Configuration{}, "acr.io/purchase:old1234")

	if _, err := d.Plan(context.Background(), &target.Desired{
		Service: "purchase",
		Version: "new5678",
		Target:  workerTarget(config.StrategyDirect),
	}); err != nil {
		t.Fatalf("an unset revision mode was refused: %v", err)
	}
}

// The declaration is checked against the app in both directions that matter.
// Either mismatch is how an app that takes requests, or one whose old revision
// keeps running, would be written without a gate.
func TestAWorkerThatIsNotShapedLikeOneIsRefused(t *testing.T) {
	cases := map[string]struct {
		cfg  *armappcontainers.Configuration
		want []string
	}{
		"has ingress": {
			cfg: &armappcontainers.Configuration{
				ActiveRevisionsMode: to.Ptr(armappcontainers.ActiveRevisionsModeSingle),
				Ingress:             &armappcontainers.Ingress{Fqdn: to.Ptr("events.example")},
			},
			want: []string{"evolve-tst-purchase-events", "has ingress", "Terraform"},
		},
		"multiple revisions": {
			cfg: &armappcontainers.Configuration{
				ActiveRevisionsMode: to.Ptr(armappcontainers.ActiveRevisionsModeMultiple),
			},
			want: []string{"evolve-tst-purchase-events", "Multiple", `revision_mode = "Single"`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for _, strategy := range []config.StrategyType{config.StrategyDirect, config.StrategyBlueGreen} {
				d := serving(t, tc.cfg, "acr.io/purchase:old1234")
				_, err := d.Plan(context.Background(), &target.Desired{
					Service: "purchase",
					Version: "new5678",
					Target:  workerTarget(strategy),
				})
				if err == nil {
					t.Fatalf("%s: accepted", strategy)
				}
				for _, w := range tc.want {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("%s: refusal does not mention %q: %v", strategy, w, err)
					}
				}
			}
		})
	}
}

// Without the declaration nothing changes: a blue-green app with no ingress is
// still refused, and the refusal now says what to write if it is a worker.
func TestAnUndeclaredWorkerIsStillRefusedAndToldWhy(t *testing.T) {
	d := serving(t, &armappcontainers.Configuration{
		ActiveRevisionsMode: to.Ptr(armappcontainers.ActiveRevisionsModeMultiple),
	}, "acr.io/purchase:old1234")

	_, err := d.Sides(context.Background(), &config.Target{
		Type:     config.TypeContainerApp,
		Name:     "evolve-tst-purchase-events",
		Strategy: &config.Strategy{Type: config.StrategyBlueGreen, Labels: config.DefaultLabels},
	})
	if err == nil {
		t.Fatal("a blue-green app with no ingress was accepted")
	}
	if !strings.Contains(err.Error(), "ingress: false") {
		t.Errorf("refusal does not point at `ingress: false`: %v", err)
	}
}
