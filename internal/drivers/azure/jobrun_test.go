package azure

import (
	"bytes"
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
	"github.com/evolve-platform/evolve-deploy/internal/hooks"
)

// jobArm answers the reads and writes of a `container-app-job` hook and records the writes.
type jobArm struct {
	image   string
	command []string
	// statuses is what each read of the execution reports; the last repeats.
	statuses []armappcontainers.JobExecutionRunningState

	patches [][]*armappcontainers.Container
	started int
	reads   int
	stopped []string
}

func (a *jobArm) driver(t *testing.T) *Driver {
	t.Helper()
	factory := fake.ServerFactory{
		JobsServer: fake.JobsServer{
			Get: func(
				_ context.Context, _, _ string, _ *armappcontainers.JobsClientGetOptions,
			) (resp azfake.Responder[armappcontainers.JobsClientGetResponse], errResp azfake.ErrorResponder) {
				resp.SetResponse(http.StatusOK, armappcontainers.JobsClientGetResponse{Job: armappcontainers.Job{
					Properties: &armappcontainers.JobProperties{Template: &armappcontainers.JobTemplate{
						Containers: []*armappcontainers.Container{{
							Name:    to.Ptr("main"),
							Image:   to.Ptr(a.image),
							Command: to.SliceOfPtrs(a.command...),
							Args:    to.SliceOfPtrs("--noinput"),
						}},
					}},
				}}, nil)
				return
			},
			BeginUpdate: func(
				_ context.Context, _, _ string, patch armappcontainers.JobPatchProperties,
				_ *armappcontainers.JobsClientBeginUpdateOptions,
			) (resp azfake.PollerResponder[armappcontainers.JobsClientUpdateResponse], errResp azfake.ErrorResponder) {
				a.patches = append(a.patches, patch.Properties.Template.Containers)
				resp.SetTerminalResponse(http.StatusOK, armappcontainers.JobsClientUpdateResponse{}, nil)
				return
			},
			BeginStopExecution: func(
				_ context.Context, _, _, exec string, _ *armappcontainers.JobsClientBeginStopExecutionOptions,
			) (resp azfake.PollerResponder[armappcontainers.JobsClientStopExecutionResponse], errResp azfake.ErrorResponder) {
				a.stopped = append(a.stopped, exec)
				resp.SetTerminalResponse(http.StatusOK, armappcontainers.JobsClientStopExecutionResponse{}, nil)
				return
			},
			BeginStart: func(
				_ context.Context, _, _ string, _ *armappcontainers.JobsClientBeginStartOptions,
			) (resp azfake.PollerResponder[armappcontainers.JobsClientStartResponse], errResp azfake.ErrorResponder) {
				a.started++
				resp.SetTerminalResponse(http.StatusOK, armappcontainers.JobsClientStartResponse{
					JobExecutionBase: armappcontainers.JobExecutionBase{Name: to.Ptr("migrate-x1")},
				}, nil)
				return
			},
		},
		ContainerAppsAPIServer: fake.ContainerAppsAPIServer{
			JobExecution: func(
				_ context.Context, _, _, _ string,
				_ *armappcontainers.ContainerAppsAPIClientJobExecutionOptions,
			) (resp azfake.Responder[armappcontainers.ContainerAppsAPIClientJobExecutionResponse], errResp azfake.ErrorResponder) {
				status := a.statuses[min(a.reads, len(a.statuses)-1)]
				a.reads++
				resp.SetResponse(http.StatusOK, armappcontainers.ContainerAppsAPIClientJobExecutionResponse{
					JobExecution: armappcontainers.JobExecution{
						Properties: &armappcontainers.JobExecutionProperties{Status: to.Ptr(status)},
					},
				}, nil)
				return
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
			Retry:     policy.RetryOptions{MaxRetries: -1},
		}},
	)
	if err != nil {
		t.Fatalf("building the driver: %v", err)
	}
	d.poll = time.Millisecond
	return d
}

func TestAJobHookRunsTheReleaseAndWaitsForIt(t *testing.T) {
	cloud := &jobArm{
		image:   "acr.io/wagtail:old",
		command: []string{"manage", "migrate"},
		statuses: []armappcontainers.JobExecutionRunningState{
			armappcontainers.JobExecutionRunningStateProcessing,
			armappcontainers.JobExecutionRunningStateRunning,
			armappcontainers.JobExecutionRunningStateSucceeded,
		},
	}
	d := cloud.driver(t)

	var out bytes.Buffer
	if err := d.RunJob(context.Background(), hooks.Job{Name: "migrate", Version: "v2"}, &out); err != nil {
		t.Fatal(err)
	}

	if len(cloud.patches) != 1 {
		t.Fatalf("wrote the job %d times, want once", len(cloud.patches))
	}
	c := cloud.patches[0][0]
	if got := *c.Image; got != "acr.io/wagtail:v2" {
		t.Errorf("image = %q", got)
	}
	// No command from the hook, so Terraform's stays — args and all.
	if len(c.Command) != 2 || len(c.Args) != 1 {
		t.Errorf("command = %v, args = %v", c.Command, c.Args)
	}
	if cloud.started != 1 || cloud.reads != 3 {
		t.Errorf("started %d, read the execution %d times", cloud.started, cloud.reads)
	}
	if !strings.Contains(out.String(), "execution migrate-x1 succeeded") {
		t.Errorf("printed %q", out.String())
	}
}

func TestAJobOnItsVersionAlreadyIsNotWritten(t *testing.T) {
	// The second apply of a release: the job already is what the run needs.
	cloud := &jobArm{
		image:    "acr.io/wagtail:v2",
		statuses: []armappcontainers.JobExecutionRunningState{armappcontainers.JobExecutionRunningStateSucceeded},
	}
	if err := cloud.driver(t).RunJob(context.Background(), hooks.Job{Name: "migrate", Version: "v2"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(cloud.patches) != 0 {
		t.Errorf("wrote the job %d times", len(cloud.patches))
	}
	if cloud.started != 1 {
		t.Errorf("started %d times — it still has to run", cloud.started)
	}
}

func TestAHookCommandIsForOneRun(t *testing.T) {
	// The next run without a command must run Terraform's, so it goes back
	// even when the run failed.
	cloud := &jobArm{
		image:    "acr.io/wagtail:old",
		command:  []string{"manage", "migrate"},
		statuses: []armappcontainers.JobExecutionRunningState{armappcontainers.JobExecutionRunningStateFailed},
	}
	err := cloud.driver(t).RunJob(context.Background(), hooks.Job{
		Name: "migrate", Version: "v2", Command: []string{"manage", "loadperms"},
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "execution migrate-x1 Failed") {
		t.Fatalf("error was %v", err)
	}

	if len(cloud.patches) != 2 {
		t.Fatalf("wrote the job %d times, want the run and the restore", len(cloud.patches))
	}
	run, restore := cloud.patches[0][0], cloud.patches[1][0]
	if got := *run.Command[1]; got != "loadperms" || run.Args != nil {
		t.Errorf("run with command %v, args %v", run.Command, run.Args)
	}
	if got := *restore.Command[1]; got != "migrate" || len(restore.Args) != 1 {
		t.Errorf("restored command %v, args %v", restore.Command, restore.Args)
	}
	// The image stays on the release: it is the command that was for one run.
	if got := *restore.Image; got != "acr.io/wagtail:v2" {
		t.Errorf("restored image = %q", got)
	}
}

func TestARunPastItsTimeoutIsStopped(t *testing.T) {
	// Walking away would leave a migration running under a release that has
	// gone ahead without it, which is what the timeout is there to prevent.
	cloud := &jobArm{
		image:    "acr.io/wagtail:v2",
		statuses: []armappcontainers.JobExecutionRunningState{armappcontainers.JobExecutionRunningStateRunning},
	}
	err := cloud.driver(t).RunJob(context.Background(), hooks.Job{
		Name: "migrate", Version: "v2", Timeout: 20 * time.Millisecond,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "ran longer than its timeout of 20ms and was stopped") {
		t.Fatalf("error was %v", err)
	}
	if len(cloud.stopped) != 1 || cloud.stopped[0] != "migrate-x1" {
		t.Errorf("stopped %v, want migrate-x1", cloud.stopped)
	}
}
