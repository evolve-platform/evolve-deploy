package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type fakeJobs struct {
	ran []Job
	// say is printed by every run, and fail makes it fail after printing.
	say  string
	fail error
}

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
		`{uses: job}`:                       "`name` is required",
		`{uses: job, with: {nam: migrate}}`: "no such option: nam",
		`{uses: job, with: {name: migrate, command: "manage migrate"}}`: "cannot unmarshal",
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

	h := jobHook(t, `{uses: job, with: {name: "suz-{{.env}}-migrate", command: [manage, migrate]}}`)
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
		[]*Hook{jobHook(t, `{uses: job, with: {name: migrate}}`)}, deploy())
	if err == nil {
		t.Fatal("a failed job did not fail the hook")
	}
	if !strings.Contains(out.String(), "[wagtail] execution suz-migrate-x1 Failed") {
		t.Errorf("printed %q", out.String())
	}
}

func TestAJobOnACloudWithoutJobsIsRefusedByTheProbe(t *testing.T) {
	h := jobHook(t, `{uses: job, with: {name: migrate}}`)
	err := Probe(context.Background(), []*Hook{h}, deploy().Data(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "has no jobs a hook can run") {
		t.Errorf("error was %v", err)
	}

	// Every other hook needs nothing from the cloud, so it passes the probe
	// on one that has no jobs.
	if err := Probe(context.Background(), []*Hook{Command("true")}, deploy().Data(), nil, nil); err != nil {
		t.Errorf("a command was refused: %v", err)
	}
}
