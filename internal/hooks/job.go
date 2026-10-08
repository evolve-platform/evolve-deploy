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
	"time"

	"gopkg.in/yaml.v3"
)

// Jobs is what a cloud offers a `uses: container-app-job`, `cloud-run-job` or
// `ecs-task`: a job that already exists, run once on a version and waited for.
//
// Declared here rather than in package target because this is the package that
// needs it, and target cannot be imported from here without a cycle. A driver
// implements it or does not; one that does not is refused at plan time.
type Jobs interface {
	// Name is the cloud, which decides which of the three names is the right
	// one. Every driver already has it.
	Name() string

	// CheckJob reads the job and reports whether RunJob could run it, without
	// writing anything. A job Terraform never created is the case it exists
	// for: that belongs in the plan, not in an `after` hook on a release that
	// already went out.
	CheckJob(ctx context.Context, j Job) error

	// RunJob puts the job on j.Version, runs it once and waits until it has
	// finished, failing when the run did. A run still going after j.Timeout is
	// stopped, and fails. What it has to say on the way goes to out, in whole
	// lines.
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

	// Service is the service whose hook this is, empty in a release-wide
	// smoke test. Only AWS reads it: ECS has no job resource, so a run needs a
	// cluster and a network to start in, and those are the service's.
	Service string
	// Target names the ECS target to take the cluster and network from, where
	// the service has none or several. AWS only.
	Target string
	// Base is the task definition family Terraform registers the job's shape
	// into, <Name>-base unless set. AWS only.
	Base string

	// Timeout is how long the run may take before it is stopped and the hook
	// fails. Zero waits for as long as the platform lets it run.
	Timeout time.Duration
}

// jobOptions are what `container-app-job` and `cloud-run-job` take.
type jobOptions struct {
	Name      string   `yaml:"name"`
	Container string   `yaml:"container"`
	Version   string   `yaml:"version"`
	Command   []string `yaml:"command"`
	Timeout   string   `yaml:"timeout"`
}

// ecsTaskOptions are jobOptions and the two things AWS needs to make up for
// having no job resource. A type of their own rather than two fields the other
// names refuse by hand, so that the schema and the decoder refuse them in the
// same words as any other option nobody has.
type ecsTaskOptions struct {
	jobOptions `yaml:",inline"`

	Target string `yaml:"target"`
	Base   string `yaml:"base"`
}

// jobClouds are the names a job goes by, and the cloud each one is.
//
// Three names for one action, because a deploy file already speaks its cloud's
// language: its targets are `container-app` or `cloud-run` or `ecs`, and a hook
// that runs the same resource as a `container-app-job` target is clearest
// called that. A neutral name would buy portability between clouds, which a
// deploy file never has.
var jobClouds = map[string]string{
	"container-app-job": "azure",
	"cloud-run-job":     "gcp",
	"ecs-task":          "aws",
}

// jobAction runs a Container Apps job, a Cloud Run job or a one-off ECS task — a
// migration before a release, a permissions load after it.
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
	// uses is the name it was written as, which every message repeats back.
	uses    string
	o       ecsTaskOptions
	timeout time.Duration
}

func parseJob(uses string) func(*yaml.Node) (Action, error) {
	return func(with *yaml.Node) (Action, error) {
		a, err := parseJobAs(uses, with)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", uses, err)
		}
		return a, nil
	}
}

func parseJobAs(uses string, with *yaml.Node) (Action, error) {
	var o ecsTaskOptions
	into := any(&o)
	if uses != "ecs-task" {
		into = &o.jobOptions
	}
	if err := decode(with, into); err != nil {
		return nil, err
	}
	if o.Name == "" {
		return nil, errors.New("`name` is required")
	}
	// Parsed now rather than at the run, so a typo is a config error and not
	// a failed `after` hook.
	var timeout time.Duration
	if o.Timeout != "" {
		var err error
		timeout, err = time.ParseDuration(o.Timeout)
		if err != nil || timeout <= 0 {
			return nil, fmt.Errorf("timeout: %q is not a duration (try 10m or 1h)", o.Timeout)
		}
	}
	// The release's own version, because that is the point: a migration run
	// against the image that is about to go out, not the one already serving.
	o.Version = cmp.Or(o.Version, "{{.version}}")
	return jobAction{uses: uses, o: o, timeout: timeout}, nil
}

func (a jobAction) Describe() string {
	line := fmt.Sprintf("%s %s on %s", a.uses, a.o.Name, a.o.Version)
	if len(a.o.Command) > 0 {
		line += ": " + strings.Join(a.o.Command, " ")
	}
	if a.o.Timeout != "" {
		line += " (at most " + a.o.Timeout + ")"
	}
	return line
}

func (a jobAction) Render(data any, funcs template.FuncMap) (Step, error) {
	b := a
	b.o.Command = slices.Clone(a.o.Command)
	fields := []*string{&b.o.Name, &b.o.Container, &b.o.Version, &b.o.Target, &b.o.Base}
	for i := range b.o.Command {
		fields = append(fields, &b.o.Command[i])
	}
	if err := render(data, funcs, fields...); err != nil {
		return Step{}, err
	}
	if b.o.Version == "" {
		// A version that rendered to nothing would retag the image to `name:`,
		// which the registry refuses only after the job has been rewritten.
		return Step{}, fmt.Errorf("%s: `version` rendered to nothing", a.uses)
	}
	// The service is read from the variables rather than taken as an option:
	// it is always the hook's own, and a smoke test, which belongs to no
	// service, has no {{.name}} to give.
	var service string
	if m, ok := data.(map[string]any); ok {
		service, _ = m["name"].(string)
	}
	return Step{line: b.Describe(), run: b.run(service), probe: b.probe(service)}, nil
}

func (a jobAction) job(service string) Job {
	return Job{
		Name:      a.o.Name,
		Container: a.o.Container,
		Version:   a.o.Version,
		Command:   a.o.Command,
		Service:   service,
		Target:    a.o.Target,
		Base:      a.o.Base,
		Timeout:   a.timeout,
	}
}

func (a jobAction) probe(service string) func(context.Context, *Exec) error {
	return func(ctx context.Context, e *Exec) error {
		if err := a.onItsCloud(e.Jobs); err != nil {
			return err
		}
		if err := e.Jobs.CheckJob(ctx, a.job(service)); err != nil {
			return fmt.Errorf("%s: %w", a.uses, err)
		}
		return nil
	}
}

func (a jobAction) run(service string) func(context.Context, *Exec) error {
	return func(ctx context.Context, e *Exec) error {
		// Probed while planning, so unreachable from the CLI. A job that
		// silently does not run is a migration that silently did not happen.
		if err := a.onItsCloud(e.Jobs); err != nil {
			return err
		}
		return e.Jobs.RunJob(ctx, a.job(service), e.Out)
	}
}

// onItsCloud refuses a name written for another cloud, and says which one this
// cloud uses.
func (a jobAction) onItsCloud(jobs Jobs) error {
	if jobs == nil {
		return fmt.Errorf("%s: this cloud has no jobs a hook can run", a.uses)
	}
	if want := jobClouds[a.uses]; jobs.Name() != want {
		for uses, cloud := range jobClouds {
			if cloud == jobs.Name() {
				return fmt.Errorf("%s is %s's; on %s this is `uses: %s`",
					a.uses, want, cloud, uses)
			}
		}
		return fmt.Errorf("%s is %s's, and this is %s", a.uses, want, jobs.Name())
	}
	return nil
}
