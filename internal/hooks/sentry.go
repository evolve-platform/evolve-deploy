package hooks

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

const sentryDefault = "https://sentry.io/api/0"

type sentryOptions struct {
	Org string `yaml:"org"`
	// Project defaults to the service name, and Projects exists for the case
	// that breaks: one deploy that is several Sentry projects.
	Project  string   `yaml:"project"`
	Projects []string `yaml:"projects"`
	// Version is what Sentry calls a release, and what the SDK in the running
	// service has to report for an error to be attributed to it.
	Version     string `yaml:"version"`
	Environment string `yaml:"environment"`
	// Repository and Commit associate the release with what is in it, which is
	// what turns a stack trace into a suspect commit.
	Repository string `yaml:"repository"`
	Commit     string `yaml:"commit"`
	Endpoint   string `yaml:"endpoint"`
	KeyEnv     string `yaml:"key_env"`
}

// sentryAction registers the release and then the deploy of it.
//
// Two calls rather than one, because they are two things Sentry knows: a
// release is the code, a deploy is the code arriving somewhere. The same
// release is deployed to tst and then to prd, and only the second call differs.
type sentryAction struct {
	o sentryOptions
}

func parseSentry(with *yaml.Node) (Action, error) {
	// Decoded bare and defaulted afterwards, so that "was this set" is a real
	// question. Defaulted first, `project` would always look set and could
	// never be refused next to `projects`.
	var o sentryOptions
	if err := decode(with, &o); err != nil {
		return nil, err
	}

	switch {
	case o.Org == "":
		return nil, fmt.Errorf("sentry: `org` is required")
	case o.Project != "" && len(o.Projects) > 0:
		return nil, fmt.Errorf("sentry: set either `project` or `projects`, not both")
	case o.Repository != "" && o.Commit == "":
		return nil, fmt.Errorf("sentry: `repository` says where to look for a commit, so it needs a `commit`")
	case o.Commit != "" && o.Repository == "":
		return nil, fmt.Errorf("sentry: `commit` is a commit in a repository, so it needs a `repository`")
	}

	// One list from here on: the two spellings are the same thing to Sentry,
	// which takes an array either way.
	if len(o.Projects) == 0 {
		o.Projects = []string{cmp.Or(o.Project, "{{.name}}")}
	}
	o.Project = ""

	o.Version = cmp.Or(o.Version, "{{.version}}")
	o.Environment = cmp.Or(o.Environment, "{{.env}}")
	o.Endpoint = cmp.Or(o.Endpoint, sentryDefault)
	o.KeyEnv = cmp.Or(o.KeyEnv, "SENTRY_AUTH_TOKEN")
	return sentryAction{o: o}, nil
}

func (a sentryAction) Describe() string {
	return fmt.Sprintf("sentry release %s deployed to %s (%s/%s)",
		a.o.Version, a.o.Environment, a.o.Org, strings.Join(a.o.Projects, ", "))
}

func (a sentryAction) Check() error {
	if _, err := token(a.o.KeyEnv); err != nil {
		return fmt.Errorf("sentry: %w", err)
	}
	return endpoint(a.o.Endpoint)
}

func (a sentryAction) Render(data any, funcs template.FuncMap) (Step, error) {
	b := a
	b.o.Projects = slices.Clone(a.o.Projects)
	fields := []*string{
		&b.o.Org, &b.o.Version, &b.o.Environment,
		&b.o.Repository, &b.o.Commit, &b.o.Endpoint,
	}
	for i := range b.o.Projects {
		fields = append(fields, &b.o.Projects[i])
	}
	if err := render(data, funcs, fields...); err != nil {
		return Step{}, err
	}
	return Step{line: b.Describe(), run: b.run}, nil
}

func (a sentryAction) run(ctx context.Context, _ *Exec) error {
	return announce(a.Describe(), a.register(ctx))
}

func (a sentryAction) register(ctx context.Context) error {
	key, err := token(a.o.KeyEnv)
	if err != nil {
		return err
	}
	header := map[string]string{"Authorization": "Bearer " + key}
	base := fmt.Sprintf("%s/organizations/%s/releases/",
		strings.TrimSuffix(a.o.Endpoint, "/"), url.PathEscape(a.o.Org))

	release := map[string]any{"version": a.o.Version, "projects": a.o.Projects}
	if a.o.Commit != "" {
		release["refs"] = []map[string]string{{
			"repository": a.o.Repository,
			"commit":     a.o.Commit,
		}}
	}

	status, said, err := postJSON(ctx, base, header, release)
	switch {
	case err != nil:
		return err
	// A release two services share is created once and reported to Sentry
	// twice, so an existing one is the normal case rather than a failure. It
	// arrives as 208 when Sentry recognises it and as a 400 that says so when
	// the refs no longer match.
	case status/100 == 2, status == 400 && strings.Contains(string(said), "already exists"):
	default:
		return refused(status, said)
	}

	deploy := map[string]string{"environment": a.o.Environment}
	status, said, err = postJSON(ctx, base+url.PathEscape(a.o.Version)+"/deploys/", header, deploy)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return refused(status, said)
	}
	return nil
}
