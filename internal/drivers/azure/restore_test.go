package azure

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3/fake"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

var day = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func ranWith(img, env string, command ...string) *armappcontainers.Container {
	c := container(appContainer, img)
	c.Env = []*armappcontainers.EnvironmentVar{{Name: to.Ptr("MODE"), Value: to.Ptr(env)}}
	c.Command = to.SliceOfPtrs(command...)
	return c
}

func revisionAt(name string, created time.Time, c *armappcontainers.Container) *armappcontainers.Revision {
	return &armappcontainers.Revision{
		Name: to.Ptr(name),
		Properties: &armappcontainers.RevisionProperties{
			CreatedTime: to.Ptr(created),
			Template:    &armappcontainers.Template{Containers: []*armappcontainers.Container{c}},
		},
	}
}

// workerWithRevisions answers the app read with the worker as it runs now and
// the revision list with what Single mode left behind.
func workerWithRevisions(t *testing.T, now *armappcontainers.Container, revisions ...*armappcontainers.Revision) *Driver {
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
							Configuration: single(),
							Template:      &armappcontainers.Template{Containers: []*armappcontainers.Container{now}},
						},
					},
				}, nil)
				return resp, azfake.ErrorResponder{}
			},
		},
		ContainerAppsRevisionsServer: fake.ContainerAppsRevisionsServer{
			NewListRevisionsPager: func(
				_, _ string,
				_ *armappcontainers.ContainerAppsRevisionsClientListRevisionsOptions,
			) azfake.PagerResponder[armappcontainers.ContainerAppsRevisionsClientListRevisionsResponse] {
				var resp azfake.PagerResponder[armappcontainers.ContainerAppsRevisionsClientListRevisionsResponse]
				resp.AddPage(http.StatusOK, armappcontainers.ContainerAppsRevisionsClientListRevisionsResponse{
					RevisionCollection: armappcontainers.RevisionCollection{Value: revisions},
				}, nil)
				return resp
			},
		},
	}
	return fakeDriver(t, &factory)
}

func runAt(name string, started time.Time, img, env string, command ...string) *armappcontainers.JobExecution {
	return &armappcontainers.JobExecution{
		Name: to.Ptr(name),
		Properties: &armappcontainers.JobExecutionProperties{
			StartTime: to.Ptr(started),
			Template: &armappcontainers.JobExecutionTemplate{
				Containers: []*armappcontainers.JobExecutionContainer{{
					Name:    to.Ptr(appContainer),
					Image:   to.Ptr(img),
					Env:     []*armappcontainers.EnvironmentVar{{Name: to.Ptr("MODE"), Value: to.Ptr(env)}},
					Command: to.SliceOfPtrs(command...),
				}},
			},
		},
	}
}

// jobWithRuns answers the job read with the template now and the execution
// list with its past runs.
func jobWithRuns(t *testing.T, now *armappcontainers.Container, runs ...*armappcontainers.JobExecution) *Driver {
	t.Helper()
	factory := fake.ServerFactory{
		JobsServer: fake.JobsServer{
			Get: func(
				_ context.Context, _, name string,
				_ *armappcontainers.JobsClientGetOptions,
			) (azfake.Responder[armappcontainers.JobsClientGetResponse], azfake.ErrorResponder) {
				var resp azfake.Responder[armappcontainers.JobsClientGetResponse]
				resp.SetResponse(http.StatusOK, armappcontainers.JobsClientGetResponse{
					Job: armappcontainers.Job{
						Name: to.Ptr(name),
						Properties: &armappcontainers.JobProperties{
							Template: &armappcontainers.JobTemplate{Containers: []*armappcontainers.Container{now}},
						},
					},
				}, nil)
				return resp, azfake.ErrorResponder{}
			},
		},
		JobsExecutionsServer: fake.JobsExecutionsServer{
			NewListPager: func(
				_, _ string,
				_ *armappcontainers.JobsExecutionsClientListOptions,
			) azfake.PagerResponder[armappcontainers.JobsExecutionsClientListResponse] {
				var resp azfake.PagerResponder[armappcontainers.JobsExecutionsClientListResponse]
				resp.AddPage(http.StatusOK, armappcontainers.JobsExecutionsClientListResponse{
					ContainerAppJobExecutions: armappcontainers.ContainerAppJobExecutions{Value: runs},
				}, nil)
				return resp
			},
		},
	}
	return fakeDriver(t, &factory)
}

func jobTarget() *config.Target {
	return &config.Target{
		Type:    config.TypeContainerJob,
		Name:    "evolve-tst-discover-products",
		Service: "discover",
	}
}

// restoredTo pulls the application container out of a restore's payload.
func restoredTo(t *testing.T, ch *target.Change) *armappcontainers.Container {
	t.Helper()
	var next []*armappcontainers.Container
	switch p := ch.Payload.(type) {
	case *appPayload:
		next = p.containers
	case *jobPayload:
		next = p.containers
	default:
		t.Fatalf("payload is %T", ch.Payload)
	}
	c := findContainer(next, appContainer)
	if c == nil {
		t.Fatal("the application container is missing from the restore")
	}
	return c
}

func envOf(c *armappcontainers.Container) string {
	for _, e := range c.Env {
		if derefString(e.Name) == "MODE" {
			return derefString(e.Value)
		}
	}
	return ""
}

// The newest revision on the version is exactly what ran there, so its
// environment and entry point come back with it — not today's config.
func TestAWorkerIsRestoredFromItsRevision(t *testing.T) {
	d := workerWithRevisions(t,
		ranWith("acr.io/purchase:new5678", "today", "node", "events"),
		revisionAt("events--a", day, ranWith("acr.io/purchase:old1234", "older", "node", "a")),
		revisionAt("events--b", day.Add(time.Hour), ranWith("acr.io/purchase:old1234", "then", "node", "b")),
		revisionAt("events--c", day.Add(2*time.Hour), ranWith("acr.io/purchase:new5678", "today", "node", "events")),
	)

	ch, err := d.PlanRestore(context.Background(), workerTarget(config.StrategyBlueGreen), "old1234")
	if err != nil {
		t.Fatal(err)
	}
	if ch == nil {
		t.Fatal("expected a change")
	}
	got := restoredTo(t, ch)
	if derefString(got.Image) != "acr.io/purchase:old1234" {
		t.Errorf("image = %s", derefString(got.Image))
	}
	if envOf(got) != "then" {
		t.Errorf("environment came from %q, want the newest revision on the version", envOf(got))
	}
	if cmd := containerCommand(got); !slices.Equal(cmd, []string{"node", "b"}) {
		t.Errorf("command = %v", cmd)
	}
	if !strings.Contains(ch.Reason, "events--b") {
		t.Errorf("reason does not name the revision: %s", ch.Reason)
	}
	if ch.FromVersion != "new5678" || ch.ToVersion != "old1234" {
		t.Errorf("versions = %s -> %s", ch.FromVersion, ch.ToVersion)
	}
}

// The platform keeps a bounded number of revisions. With none left on the
// version, the tag goes back on its own and the reason says what did not.
func TestAWorkerWithNoRevisionLeftGetsTheTagOnly(t *testing.T) {
	d := workerWithRevisions(t,
		ranWith("acr.io/purchase:new5678", "today", "node", "events"),
		revisionAt("events--c", day, ranWith("acr.io/purchase:new5678", "today", "node", "events")),
	)

	ch, err := d.PlanRestore(context.Background(), workerTarget(config.StrategyBlueGreen), "old1234")
	if err != nil {
		t.Fatal(err)
	}
	got := restoredTo(t, ch)
	if derefString(got.Image) != "acr.io/purchase:old1234" {
		t.Errorf("image = %s", derefString(got.Image))
	}
	if envOf(got) != "today" {
		t.Errorf("environment = %q, want today's kept", envOf(got))
	}
	if !strings.Contains(ch.Reason, "image only") {
		t.Errorf("reason does not say it restored the image only: %s", ch.Reason)
	}
}

func TestAJobIsRestoredFromItsLastRunOnTheVersion(t *testing.T) {
	d := jobWithRuns(t,
		ranWith("acr.io/discover:new5678", "today", "node", "sync"),
		runAt("products-1", day, "acr.io/discover:old1234", "older", "node", "a"),
		runAt("products-2", day.Add(time.Hour), "acr.io/discover:old1234", "then", "node", "b"),
		runAt("products-3", day.Add(2*time.Hour), "acr.io/discover:new5678", "today", "node", "sync"),
	)

	ch, err := d.PlanRestore(context.Background(), jobTarget(), "old1234")
	if err != nil {
		t.Fatal(err)
	}
	got := restoredTo(t, ch)
	if derefString(got.Image) != "acr.io/discover:old1234" {
		t.Errorf("image = %s", derefString(got.Image))
	}
	if envOf(got) != "then" {
		t.Errorf("environment came from %q, want the newest run on the version", envOf(got))
	}
	if cmd := containerCommand(got); !slices.Equal(cmd, []string{"node", "b"}) {
		t.Errorf("command = %v", cmd)
	}
	if !strings.Contains(ch.Reason, "products-2") {
		t.Errorf("reason does not name the run: %s", ch.Reason)
	}
}

// A job that never ran on the version left no record of it: the tag goes back
// and the environment and command stay today's, which the reason says.
func TestAJobThatNeverRanOnTheVersionGetsTheTagOnly(t *testing.T) {
	d := jobWithRuns(t, ranWith("acr.io/discover:new5678", "today", "node", "sync"))

	ch, err := d.PlanRestore(context.Background(), jobTarget(), "old1234")
	if err != nil {
		t.Fatal(err)
	}
	got := restoredTo(t, ch)
	if derefString(got.Image) != "acr.io/discover:old1234" {
		t.Errorf("image = %s", derefString(got.Image))
	}
	if envOf(got) != "today" {
		t.Errorf("environment = %q, want today's kept", envOf(got))
	}
	if cmd := containerCommand(got); !slices.Equal(cmd, []string{"node", "sync"}) {
		t.Errorf("command = %v, want today's kept", cmd)
	}
	if !strings.Contains(ch.Reason, "image only") {
		t.Errorf("reason does not say it restored the image only: %s", ch.Reason)
	}
}

// A rollback run twice writes nothing the second time.
func TestARiderAlreadyOnTheVersionIsLeftAlone(t *testing.T) {
	d := jobWithRuns(t, ranWith("acr.io/discover:old1234", "today", "node", "sync"))

	ch, err := d.PlanRestore(context.Background(), jobTarget(), "old1234")
	if err != nil {
		t.Fatal(err)
	}
	if ch != nil {
		t.Errorf("planned a change for a rider already on the version: %s", ch.Reason)
	}

	v, err := d.RiderVersion(context.Background(), jobTarget())
	if err != nil || v != "old1234" {
		t.Errorf("RiderVersion = %q, %v", v, err)
	}
}

// A function app swaps slots and has no record of past versions here. That is
// a known limit the caller turns into advice, so it is not-implemented rather
// than a failure.
func TestAFunctionAppCannotBeRestored(t *testing.T) {
	d := &Driver{}
	_, err := d.PlanRestore(context.Background(),
		&config.Target{Type: config.TypeFunctionApp, Name: "fn"}, "old1234")

	var unsupported *target.ErrNotImplemented
	if !errors.As(err, &unsupported) {
		t.Errorf("err = %v, want ErrNotImplemented", err)
	}
}
