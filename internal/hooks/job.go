package hooks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// Jobs is what a cloud offers a `uses: job`: a job that already exists, run once
// on a version and waited for.
//
// Declared here rather than in package target because this is the package that
// needs it, and target cannot be imported from here without a cycle. A driver
// implements it or does not; one that does not is refused at plan time.
type Jobs interface {
	// CheckJob reads the job and reports whether RunJob could run it, without
	// writing anything. A job Terraform never created is the case it exists
	// for: that belongs in the plan, not in an `after` hook on a release that
	// already went out.
	CheckJob(ctx context.Context, j Job) error

	// RunJob puts the job on j.Version, runs it once and waits until it has
	// finished, failing when the run did. What it has to say on the way goes to
	// out, in whole lines.
	RunJob(ctx context.Context, j Job, out io.Writer) error
}

// Job is one run of a job, with its variables filled in.
type Job struct {
	Name string
	// Container picks the application container in a job with sidecars, the
	// same way a target's `container` does. Empty means the driver's default.
	Container string
	Version   string
	// Command replaces the job's entry point for this one run. Empty runs
	// whatever the job was declared with.
	Command []string
}

type jobOptions struct {
	Name      string   `yaml:"name"`
	Container string   `yaml:"container"`
	Version   string   `yaml:"version"`
	Command   []string `yaml:"command"`
}

// jobAction runs a Container Apps job or a Cloud Run job — a migration before a
// release, a permissions load after it.
//
// Not a command line, because the command line for this is several calls with a
// poll in the middle — start the execution, read its status until it stops
// changing, tell a failed run from a stopped one — written once per cloud. And
// the version it has to run on is the one value the tool knows and the shell
// does not have to.
//
// The job is Terraform's: its environment, its secrets, its identity, its
// timeout. This changes the image and, for one run, the command, and never
// creates one that is not there.
type jobAction struct {
	o jobOptions
}

func parseJob(with *yaml.Node) (Action, error) {
	var o jobOptions
	if err := decode(with, &o); err != nil {
		return nil, err
	}
	if o.Name == "" {
		return nil, errors.New("job: `name` is required")
	}
	// The release's own version, because that is the point: a migration run
	// against the image that is about to go out, not the one already serving.
	o.Version = cmp.Or(o.Version, "{{.version}}")
	return jobAction{o: o}, nil
}

func (a jobAction) Describe() string {
	line := fmt.Sprintf("job %s on %s", a.o.Name, a.o.Version)
	if len(a.o.Command) > 0 {
		line += ": " + strings.Join(a.o.Command, " ")
	}
	return line
}

func (a jobAction) Render(data any, funcs template.FuncMap) (Step, error) {
	b := a
	b.o.Command = slices.Clone(a.o.Command)
	fields := []*string{&b.o.Name, &b.o.Container, &b.o.Version}
	for i := range b.o.Command {
		fields = append(fields, &b.o.Command[i])
	}
	if err := render(data, funcs, fields...); err != nil {
		return Step{}, err
	}
	if b.o.Version == "" {
		// A version that rendered to nothing would retag the image to `name:`,
		// which the registry refuses only after the job has been rewritten.
		return Step{}, errors.New("job: `version` rendered to nothing")
	}
	return Step{line: b.Describe(), run: b.run, probe: b.probe}, nil
}

func (a jobAction) job() Job {
	return Job{
		Name:      a.o.Name,
		Container: a.o.Container,
		Version:   a.o.Version,
		Command:   a.o.Command,
	}
}

func (a jobAction) probe(ctx context.Context, e *Exec) error {
	if e.Jobs == nil {
		return errNoJobs
	}
	if err := e.Jobs.CheckJob(ctx, a.job()); err != nil {
		return fmt.Errorf("job: %w", err)
	}
	return nil
}

func (a jobAction) run(ctx context.Context, e *Exec) error {
	if e.Jobs == nil {
		// Probed while planning, so unreachable from the CLI. A job that
		// silently does not run is a migration that silently did not happen.
		return errNoJobs
	}
	return e.Jobs.RunJob(ctx, a.job(), e.Out)
}

var errNoJobs = errors.New("job: this cloud has no jobs a hook can run " +
	"— `uses: job` works on azure and gcp")
