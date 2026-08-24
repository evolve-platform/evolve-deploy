package azure

import (
	"context"
	"errors"
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

// The wait is the code that decides whether a rollout failed, and every one of
// its verdicts is reached from three ARM reads: the app, the revision it is
// promoting, and the replicas under that. So the whole of it is tested through
// the SDK's own fake transport rather than against extracted helpers — what is
// worth pinning down here is the sequence of readings, not the arithmetic.

// poll is what the platform says on one time round the loop.
//
// The last entry of a script repeats for as long as the wait keeps asking,
// which is what lets a test drive the loop to its deadline without writing out
// a poll per five seconds of it.
type poll struct {
	// latest is the revision the app is trying to promote and ready the newest
	// one that made it. Equal, and non-empty, is what "ready" means.
	latest, ready string
	state         *armappcontainers.ContainerAppProvisioningState

	// noProperties reproduces an app read that comes back without them, which
	// is a refusal rather than a state to wait through.
	noProperties bool

	// revision and replicas are what the two reads under the app report.
	revision *armappcontainers.RevisionProperties
	replicas []*armappcontainers.Replica

	// The three reads can each fail on their own. Only the first is fatal: a
	// revision created moments ago is allowed to 404 while ARM catches up.
	appErr, revErr, repErr error
}

// waiting builds a driver whose ARM calls are answered from the script, and
// returns a counter of how many times round the loop the wait went.
//
// The app read is what counts a poll, and the two reads under it are answered
// from the same entry, so one script line is one pass of the loop however many
// calls that pass happens to make.
func waiting(t *testing.T, script ...poll) (*Driver, *int) {
	t.Helper()
	if len(script) == 0 {
		t.Fatal("a script needs at least one poll")
	}

	polls := 0
	at := func() poll {
		i := polls - 1
		if i < 0 {
			i = 0
		}
		if i >= len(script) {
			i = len(script) - 1
		}
		return script[i]
	}

	factory := fake.ServerFactory{
		ContainerAppsServer: fake.ContainerAppsServer{
			Get: func(
				_ context.Context, _, _ string,
				_ *armappcontainers.ContainerAppsClientGetOptions,
			) (azfake.Responder[armappcontainers.ContainerAppsClientGetResponse], azfake.ErrorResponder) {
				polls++
				p := at()

				var resp azfake.Responder[armappcontainers.ContainerAppsClientGetResponse]
				var errResp azfake.ErrorResponder
				if p.appErr != nil {
					errResp.SetError(p.appErr)
					return resp, errResp
				}

				app := armappcontainers.ContainerApp{}
				if !p.noProperties {
					app.Properties = &armappcontainers.ContainerAppProperties{
						LatestRevisionName:      to.Ptr(p.latest),
						LatestReadyRevisionName: to.Ptr(p.ready),
						ProvisioningState:       p.state,
					}
				}
				resp.SetResponse(http.StatusOK,
					armappcontainers.ContainerAppsClientGetResponse{ContainerApp: app}, nil)
				return resp, errResp
			},
		},
		ContainerAppsRevisionsServer: fake.ContainerAppsRevisionsServer{
			GetRevision: func(
				_ context.Context, _, _, _ string,
				_ *armappcontainers.ContainerAppsRevisionsClientGetRevisionOptions,
			) (azfake.Responder[armappcontainers.ContainerAppsRevisionsClientGetRevisionResponse], azfake.ErrorResponder) {
				p := at()

				var resp azfake.Responder[armappcontainers.ContainerAppsRevisionsClientGetRevisionResponse]
				var errResp azfake.ErrorResponder
				if p.revErr != nil {
					errResp.SetError(p.revErr)
					return resp, errResp
				}
				resp.SetResponse(http.StatusOK,
					armappcontainers.ContainerAppsRevisionsClientGetRevisionResponse{
						Revision: armappcontainers.Revision{Properties: p.revision},
					}, nil)
				return resp, errResp
			},
		},
		ContainerAppsRevisionReplicasServer: fake.ContainerAppsRevisionReplicasServer{
			ListReplicas: func(
				_ context.Context, _, _, _ string,
				_ *armappcontainers.ContainerAppsRevisionReplicasClientListReplicasOptions,
			) (azfake.Responder[armappcontainers.ContainerAppsRevisionReplicasClientListReplicasResponse], azfake.ErrorResponder) {
				p := at()

				var resp azfake.Responder[armappcontainers.ContainerAppsRevisionReplicasClientListReplicasResponse]
				var errResp azfake.ErrorResponder
				if p.repErr != nil {
					errResp.SetError(p.repErr)
					return resp, errResp
				}
				resp.SetResponse(http.StatusOK,
					armappcontainers.ContainerAppsRevisionReplicasClientListReplicasResponse{
						ReplicaCollection: armappcontainers.ReplicaCollection{Value: p.replicas},
					}, nil)
				return resp, errResp
			},
		},
	}

	d, err := newDriver(
		&config.File{Cloud: config.CloudConfig{
			Subscription:  "00000000-0000-0000-0000-000000000000",
			ResourceGroup: "rg",
		}},
		&azfake.TokenCredential{},
		&arm.ClientOptions{ClientOptions: azcore.ClientOptions{
			Transport: fake.NewServerFactoryTransport(&factory),
			// A read that fails is one of the things under test here, and the
			// default policy would retry it with a backoff — which would turn a
			// millisecond test into a several-second one and hide the fact that
			// the wait carried on by itself.
			Retry: policy.RetryOptions{MaxRetries: -1},
		}},
	)
	if err != nil {
		t.Fatalf("building the driver: %v", err)
	}

	// The poll interval is the reason this is a field rather than the constant
	// it defaults to: at five seconds a three-strike test would take fifteen.
	d.poll = time.Millisecond

	return d, &polls
}

// starting is a revision that reports nothing wrong, which is what every deploy
// looks like while it is working.
func starting() *armappcontainers.RevisionProperties {
	return &armappcontainers.RevisionProperties{}
}

// degraded is a suspicion rather than a verdict: it is also what a container
// that has not finished starting looks like for a poll or two.
func degraded() *armappcontainers.RevisionProperties {
	return &armappcontainers.RevisionProperties{
		RunningState: to.Ptr(armappcontainers.RevisionRunningStateDegraded),
	}
}

func TestWaitReadyReturnsWhenTheReadyRevisionCatchesUp(t *testing.T) {
	d, polls := waiting(t,
		poll{latest: "app--a", revision: starting()},
		poll{latest: "app--a", ready: "app--a"},
	)

	if err := d.waitReady(context.Background(), "app", time.Minute); err != nil {
		t.Fatalf("expected the wait to succeed, got %v", err)
	}
	if *polls != 2 {
		t.Errorf("expected the wait to poll twice, it polled %d times", *polls)
	}
}

func TestWaitReadyFailsOnATerminalProvisioningState(t *testing.T) {
	for _, state := range []armappcontainers.ContainerAppProvisioningState{
		armappcontainers.ContainerAppProvisioningStateFailed,
		armappcontainers.ContainerAppProvisioningStateCanceled,
	} {
		d, polls := waiting(t, poll{latest: "app--a", state: to.Ptr(state)})

		err := d.waitReady(context.Background(), "app", time.Minute)
		if err == nil {
			t.Fatalf("%s: expected the wait to fail", state)
		}
		// The app's own state is the platform's verdict and it does not change
		// its mind, so there is nothing to wait for and nothing to strike.
		if want := "app went to " + string(state); err.Error() != want {
			t.Errorf("%s: got %q, want %q", state, err, want)
		}
		if *polls != 1 {
			t.Errorf("%s: expected one poll, got %d", state, *polls)
		}
	}
}

// classifyRevision, weighReplicas and describeReplicas each have their own
// tests over in containerapp_test.go. What those cannot show is what the loop
// around them does with a verdict, which is the whole of the difference between
// a deploy that fails in ten seconds and one that fails in ten minutes.
func TestACertainVerdictSkipsTheStrikes(t *testing.T) {
	d, polls := waiting(t, poll{
		latest: "app--a",
		revision: &armappcontainers.RevisionProperties{
			ProvisioningState: to.Ptr(armappcontainers.RevisionProvisioningStateFailed),
			ProvisioningError: to.Ptr("manifest for purchase:abc not found"),
		},
	})

	err := d.waitReady(context.Background(), "app", time.Minute)
	if err == nil {
		t.Fatal("expected the wait to fail")
	}
	// One poll, not three: the platform has stopped trying, so there is nothing
	// for a second opinion to add.
	if *polls != 1 {
		t.Errorf("expected one poll, got %d", *polls)
	}
	want := "revision app--a failed to provision: manifest for purchase:abc not found"
	if err.Error() != want {
		t.Errorf("got %q, want %q", err, want)
	}
}

func TestACrashLoopEndsTheWaitOnTheFirstPoll(t *testing.T) {
	d, polls := waiting(t, poll{
		latest:   "app--a",
		revision: degraded(),
		replicas: []*armappcontainers.Replica{replica(replicaContainer(
			"main", false, armappcontainers.ContainerAppContainerRunningStateWaiting,
			"Container is waiting with reason: CrashLoopBackOff on legion.", crashLoopRestarts+1))},
	})

	err := d.waitReady(context.Background(), "app", time.Minute)
	if err == nil {
		t.Fatal("expected the wait to fail")
	}
	// The revision itself only reported Degraded, which is a suspicion and
	// would have taken three polls. The container under it is what makes this
	// certain on the first.
	if *polls != 1 {
		t.Errorf("expected one poll, got %d", *polls)
	}
	if !strings.HasPrefix(err.Error(), "revision app--a never started") {
		t.Errorf("expected the container's verdict to win, got %q", err)
	}
	// logsCommand has no test of its own, and it is the reason the message says
	// more than "it crashed": the tool cannot read the container's output, but
	// it knows every argument needed to go and look.
	if want := "az containerapp logs show -g rg -n app --revision app--a --container main"; !strings.Contains(err.Error(), want) {
		t.Errorf("expected %q in %q", want, err)
	}
}

func TestThreeStrikesEndTheWait(t *testing.T) {
	d, polls := waiting(t, poll{latest: "app--a", revision: degraded()})

	err := d.waitReady(context.Background(), "app", time.Minute)
	if err == nil {
		t.Fatal("expected the wait to fail")
	}
	if want := "revision app--a is Degraded"; err.Error() != want {
		t.Errorf("got %q, want %q", err, want)
	}
	// One bad sighting is a container that has not finished starting; three in
	// a row is one that is not going to.
	if *polls != unhealthyStrikes {
		t.Errorf("expected %d polls, got %d", unhealthyStrikes, *polls)
	}
}

func TestAGoodPollResetsTheStrikes(t *testing.T) {
	// Four degraded readings in all, but never three consecutively — which is
	// what a container being restarted looks like on its way up. Without the
	// reset the fourth would end a release that is going fine.
	d, polls := waiting(t,
		poll{latest: "app--a", revision: degraded()},
		poll{latest: "app--a", revision: degraded()},
		poll{latest: "app--a", revision: starting()},
		poll{latest: "app--a", revision: degraded()},
		poll{latest: "app--a", revision: degraded()},
		poll{latest: "app--a", ready: "app--a"},
	)

	if err := d.waitReady(context.Background(), "app", time.Minute); err != nil {
		t.Fatalf("expected the wait to succeed, got %v", err)
	}
	if *polls != 6 {
		t.Errorf("expected six polls, got %d", *polls)
	}
}

func TestTheDeadlineSaysWhatTheRevisionLastReported(t *testing.T) {
	// Degraded then clean, repeating: enough to record a reason, never enough
	// consecutively to be a verdict. Nothing conclusive ever happens, so the
	// deadline is the only way out — which is what it is for.
	d, _ := waiting(t,
		poll{latest: "app--a", state: to.Ptr(armappcontainers.ContainerAppProvisioningStateSucceeded),
			revision: degraded()},
		poll{latest: "app--a", state: to.Ptr(armappcontainers.ContainerAppProvisioningStateSucceeded),
			revision: starting()},
	)

	err := d.waitReady(context.Background(), "app", time.Millisecond)
	if err == nil {
		t.Fatal("expected the wait to time out")
	}
	for _, want := range []string{
		"revision app--a did not become ready within 1ms",
		"ready revision is (none)",
		"state Succeeded",
		"the revision last reported that it is Degraded",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q in %q", want, err)
		}
	}
}

func TestTheDeadlineSaysNothingItWasNotTold(t *testing.T) {
	d, _ := waiting(t, poll{
		latest:   "app--a",
		state:    to.Ptr(armappcontainers.ContainerAppProvisioningStateSucceeded),
		revision: starting(),
	})

	err := d.waitReady(context.Background(), "app", time.Millisecond)
	if err == nil {
		t.Fatal("expected the wait to time out")
	}
	// Nothing ever reported badly, so there is no last reason to quote. Making
	// one up out of the clean readings would send whoever reads this looking in
	// the wrong place.
	if strings.Contains(err.Error(), "last reported") {
		t.Errorf("expected no reason to be quoted, got %q", err)
	}
}

func TestAReadUnderTheAppIsNeverAVerdict(t *testing.T) {
	// The revision is created moments before the first poll, so a 404 while ARM
	// catches up — or a throttled call — must not fail a deploy that is going
	// fine. It leaves the deadline as the backstop it is meant to be.
	d, polls := waiting(t, poll{
		latest: "app--a",
		state:  to.Ptr(armappcontainers.ContainerAppProvisioningStateSucceeded),
		revErr: errors.New("ResourceNotFound"),
		repErr: errors.New("TooManyRequests"),
	})

	err := d.waitReady(context.Background(), "app", 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected the wait to time out")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("expected a deadline, got %q", err)
	}
	if *polls < 2 {
		t.Errorf("expected the wait to carry on past the failed read, it polled %d times", *polls)
	}
}

func TestAFailedAppReadEndsTheWait(t *testing.T) {
	// The app read is the one that cannot be shrugged off: without it the wait
	// has nothing at all to go on.
	d, polls := waiting(t, poll{appErr: errors.New("SubscriptionNotFound")})

	err := d.waitReady(context.Background(), "app", time.Minute)
	if err == nil {
		t.Fatal("expected the wait to fail")
	}
	if !strings.Contains(err.Error(), "waiting for app") {
		t.Errorf("expected the app to be named, got %q", err)
	}
	if *polls != 1 {
		t.Errorf("expected one poll, got %d", *polls)
	}
}

func TestAnAppWithNoPropertiesIsRefused(t *testing.T) {
	d, _ := waiting(t, poll{noProperties: true})

	err := d.waitReady(context.Background(), "app", time.Minute)
	if err == nil {
		t.Fatal("expected the wait to fail")
	}
	if want := "waiting for app: no properties returned"; err.Error() != want {
		t.Errorf("got %q, want %q", err, want)
	}
}

func TestCancellationEndsTheWait(t *testing.T) {
	// An interrupted deploy stops waiting; the rollout already handed to the
	// cloud carries on and is picked up by the next run.
	d, _ := waiting(t, poll{latest: "app--a", revision: starting()})
	d.poll = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(10*time.Millisecond, cancel)

	err := d.waitReady(ctx, "app", time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected the wait to be cancelled, got %v", err)
	}
}
