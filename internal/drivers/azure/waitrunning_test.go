package azure

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3/fake"

	"github.com/evolve-platform/evolve-deploy/internal/config"
)

// staging builds a driver whose revision reads are answered from the script,
// one entry per read, the last repeating. waitRunning reads only the revision
// and the replicas under it, so the app-driven harness in waitready_test.go
// cannot count its polls.
func staging(t *testing.T, script ...*armappcontainers.RevisionProperties) (*Driver, *int) {
	t.Helper()

	reads := 0
	factory := fake.ServerFactory{
		ContainerAppsRevisionsServer: fake.ContainerAppsRevisionsServer{
			GetRevision: func(
				_ context.Context, _, _, _ string,
				_ *armappcontainers.ContainerAppsRevisionsClientGetRevisionOptions,
			) (azfake.Responder[armappcontainers.ContainerAppsRevisionsClientGetRevisionResponse], azfake.ErrorResponder) {
				i := min(reads, len(script)-1)
				reads++

				var resp azfake.Responder[armappcontainers.ContainerAppsRevisionsClientGetRevisionResponse]
				resp.SetResponse(http.StatusOK,
					armappcontainers.ContainerAppsRevisionsClientGetRevisionResponse{
						Revision: armappcontainers.Revision{Properties: script[i]},
					}, nil)
				return resp, azfake.ErrorResponder{}
			},
		},
		ContainerAppsRevisionReplicasServer: fake.ContainerAppsRevisionReplicasServer{
			ListReplicas: func(
				_ context.Context, _, _, _ string,
				_ *armappcontainers.ContainerAppsRevisionReplicasClientListReplicasOptions,
			) (azfake.Responder[armappcontainers.ContainerAppsRevisionReplicasClientListReplicasResponse], azfake.ErrorResponder) {
				var resp azfake.Responder[armappcontainers.ContainerAppsRevisionReplicasClientListReplicasResponse]
				resp.SetResponse(http.StatusOK,
					armappcontainers.ContainerAppsRevisionReplicasClientListReplicasResponse{}, nil)
				return resp, azfake.ErrorResponder{}
			},
		},
	}

	d := fakeDriver(t, &factory)
	return d, &reads
}

func fakeDriver(t *testing.T, factory *fake.ServerFactory) *Driver {
	t.Helper()
	d, err := newDriver(
		&config.File{Cloud: config.CloudConfig{
			Subscription:  "00000000-0000-0000-0000-000000000000",
			ResourceGroup: "rg",
		}},
		&azfake.TokenCredential{},
		&arm.ClientOptions{ClientOptions: azcore.ClientOptions{
			Transport: fake.NewServerFactoryTransport(factory),
			Retry:     policy.RetryOptions{MaxRetries: -1},
		}},
	)
	if err != nil {
		t.Fatalf("building the driver: %v", err)
	}
	d.poll = time.Millisecond
	return d
}

func revisionIn(
	running armappcontainers.RevisionRunningState,
	health armappcontainers.RevisionHealthState,
) *armappcontainers.RevisionProperties {
	return &armappcontainers.RevisionProperties{
		Active:       to.Ptr(true),
		RunningState: to.Ptr(running),
		HealthState:  to.Ptr(health),
	}
}

func TestAHealthyRevisionAtZeroReplicasIsStaged(t *testing.T) {
	d, reads := staging(t,
		revisionIn(revisionScaledToZero, armappcontainers.RevisionHealthStateHealthy))

	if err := d.waitRunning(context.Background(), "app", "app--a", time.Minute); err != nil {
		t.Fatalf("expected the wait to succeed, got %v", err)
	}
	if *reads != 1 {
		t.Errorf("expected one read, got %d", *reads)
	}
}

func TestAScaledToZeroRevisionThatIsNotHealthyIsNotAccepted(t *testing.T) {
	d, _ := staging(t,
		revisionIn(revisionScaledToZero, armappcontainers.RevisionHealthStateNone))

	err := d.waitRunning(context.Background(), "app", "app--a", 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected the wait to time out")
	}
	if want := "revision app--a was not running within 20ms"; err.Error() != want {
		t.Errorf("got %q, want %q", err, want)
	}
}

func TestAWriteThatNeverFinishesIsGivenUpOn(t *testing.T) {
	factory := fake.ServerFactory{
		ContainerAppsServer: fake.ContainerAppsServer{
			BeginUpdate: func(
				_ context.Context, _, _ string, _ armappcontainers.ContainerApp,
				_ *armappcontainers.ContainerAppsClientBeginUpdateOptions,
			) (azfake.PollerResponder[armappcontainers.ContainerAppsClientUpdateResponse], azfake.ErrorResponder) {
				var resp azfake.PollerResponder[armappcontainers.ContainerAppsClientUpdateResponse]
				resp.AddNonTerminalResponse(http.StatusAccepted, nil)
				resp.AddNonTerminalResponse(http.StatusAccepted, nil)
				resp.SetTerminalResponse(http.StatusOK,
					armappcontainers.ContainerAppsClientUpdateResponse{}, nil)
				return resp, azfake.ErrorResponder{}
			},
		},
	}
	d := fakeDriver(t, &factory)
	// Shorter than the poller's own minimum interval, so the deadline is
	// reached while it is still waiting to ask again.
	d.writeTimeout = 50 * time.Millisecond

	started := time.Now()
	err := d.patchTraffic(context.Background(), "app", nil)
	if err == nil {
		t.Fatal("expected the write to be given up on")
	}
	if !strings.Contains(err.Error(), "still in progress after 50ms") {
		t.Errorf("expected the error to say the write is still in progress, got %q", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("expected the deadline to end the wait, it took %s", elapsed)
	}
}
