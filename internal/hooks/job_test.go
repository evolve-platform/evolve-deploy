package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type fakeJobs struct {
	ran []Job
	// say is printed by every run, and fail makes it fail after printing.
	say  string
	fail error
}

func (f *fakeJobs) Name() string { return "azure" }

func (f *fakeJobs) CheckJob(context.Context, Job) error { return nil }

func (f *fakeJobs) RunJob(_ context.Context, j Job, out io.Writer) error {
	f.ran = append(f.ran, j)
	fmt.Fprintln(out, f.say)
	return f.fail
}

func jobHook(t *testing.T, doc string) *Hook {
	t.Helper()
	var h Hook
	if err := yaml.Unmarshal([]byte(doc), &h); err != nil {
		t.Fatal(err)
	}
	return &h
}

func TestAJobNeedsAName(t *testing.T) {
	for doc, want := range map[string]string{
		`{uses: container-app-job}`:                                                   "`name` is required",
		`{uses: container-app-job, with: {nam: migrate}}`:                             "no such option: nam",
		`{uses: container-app-job, with: {name: migrate, command: "manage migrate"}}`: "cannot unmarshal",
		`{uses: container-app-job, with: {name: migrate, timeout: 10}}`:               "is not a duration",
		`{uses: container-app-job, with: {name: migrate, timeout: -5m}}`:              "is not a duration",
		// What AWS needs to make up for having no job resource, and nothing
		// else does.
		`{uses: cloud-run-job, with: {name: migrate, base: migrate-base}}`:  "no such option: base",
		`{uses: container-app-job, with: {name: migrate, target: ask-web}}`: "no such option: target",
	} {
		_, err := jobHook(t, doc).Action()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error was %v, want it to mention %q", doc, err, want)
		}
	}
}

func TestAJobRunsOnTheVersionBeingReleased(t *testing.T) {
	jobs := &fakeJobs{say: "Applying wagtailcore.0094... OK"}
	var out bytes.Buffer
	r := &Runner{Out: &out, Jobs: jobs}

	h := jobHook(t, `{uses: container-app-job, with: {name: "suz-{{.env}}-migrate", command: [manage, migrate], timeout: 10m}}`)
	if err := r.Run(context.Background(), "wagtail", "before", []*Hook{h}, deploy()); err != nil {
		t.Fatal(err)
	}

	if len(jobs.ran) != 1 {
		t.Fatalf("ran %d jobs", len(jobs.ran))
	}
	j := jobs.ran[0]
	if j.Name != "suz-tst-migrate" || j.Version != "abc1234" {
		t.Errorf("ran %+v", j)
	}
	if j.Timeout != 10*time.Minute {
		t.Errorf("timeout = %s", j.Timeout)
	}
	// The hook's own service, which is where AWS finds a network to run in.
	if j.Service != "purchase" {
		t.Errorf("service = %q", j.Service)
	}
	if strings.Join(j.Command, " ") != "manage migrate" {
		t.Errorf("command = %q", j.Command)
	}
	// Quiet on success, like every other hook: the migration's own log is not
	// the answer to what was deployed.
	if out.Len() != 0 {
		t.Errorf("a job that succeeded printed %q", out.String())
	}
}

func TestAFailedJobPrintsWhatItSaid(t *testing.T) {
	jobs := &fakeJobs{say: "execution suz-migrate-x1 Failed", fail: errors.New("boom")}
	var out bytes.Buffer
	r := &Runner{Out: &out, Jobs: jobs}

	err := r.Run(context.Background(), "wagtail", "before",
		[]*Hook{jobHook(t, `{uses: container-app-job, with: {name: migrate}}`)}, deploy())
	if err == nil {
		t.Fatal("a failed job did not fail the hook")
	}
	if !strings.Contains(out.String(), "[wagtail] execution suz-migrate-x1 Failed") {
		t.Errorf("printed %q", out.String())
	}
}

func TestAJobOnACloudWithoutJobsIsRefusedByTheProbe(t *testing.T) {
	h := jobHook(t, `{uses: container-app-job, with: {name: migrate}}`)
	err := Probe(context.Background(), []*Hook{h}, deploy().Data(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "container-app-job: this cloud has no jobs a hook can run") {
		t.Errorf("error was %v", err)
	}

	// Every other hook needs nothing from the cloud, so it passes the probe
	// on one that has no jobs.
	if err := Probe(context.Background(), []*Hook{Command("true")}, deploy().Data(), nil, nil); err != nil {
		t.Errorf("a command was refused: %v", err)
	}
}

func TestAJobNamedForAnotherCloudIsRefused(t *testing.T) {
	h := jobHook(t, `{uses: cloud-run-job, with: {name: migrate}}`)
	err := Probe(context.Background(), []*Hook{h}, deploy().Data(), nil, &fakeJobs{})
	if err == nil || !strings.Contains(err.Error(), "cloud-run-job is gcp's; on azure this is `uses: container-app-job`") {
		t.Errorf("error was %v", err)
	}
}
