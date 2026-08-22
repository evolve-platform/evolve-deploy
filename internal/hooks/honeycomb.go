package hooks

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// honeycombDefault is the US endpoint. The EU one is a different host rather
// than a different path, which is why this is a whole base url and not a
// region name to look up in a table that would then need maintaining.
const honeycombDefault = "https://api.honeycomb.io"

type honeycombOptions struct {
	// Dataset is the dataset the marker lands on. `__all__` puts it on the
	// whole environment, which is what a service with no dataset of its own
	// wants.
	Dataset string `yaml:"dataset"`
	Message string `yaml:"message"`
	// Type groups markers so they can be given one colour in the UI. Deploys
	// are the only kind this tool makes, so it is what it defaults to.
	Type     string `yaml:"type"`
	URL      string `yaml:"url"`
	Endpoint string `yaml:"endpoint"`
	KeyEnv   string `yaml:"key_env"`
}

// honeycombAction writes a deploy marker, so a graph that changes shape at
// 14:03 says what happened at 14:03.
type honeycombAction struct {
	o honeycombOptions
}

func parseHoneycomb(with *yaml.Node) (Action, error) {
	o := honeycombOptions{
		Message:  "{{.name}} {{.version}}",
		Type:     "deploy",
		Endpoint: honeycombDefault,
		KeyEnv:   "HONEYCOMB_API_KEY",
	}
	if err := decode(with, &o); err != nil {
		return nil, err
	}
	if o.Dataset == "" {
		return nil, fmt.Errorf("honeycomb: `dataset` is required (`__all__` marks the whole environment)")
	}
	return honeycombAction{o: o}, nil
}

func (a honeycombAction) Describe() string {
	return fmt.Sprintf("honeycomb marker on %s: %s", a.o.Dataset, a.o.Message)
}

// Check refuses a marker that has nowhere to go, while there is still a plan to
// refuse. The key is the one that is actually forgotten — a pipeline that gained
// a second environment and only gave the first one its secret.
func (a honeycombAction) Check() error {
	if _, err := token(a.o.KeyEnv); err != nil {
		return fmt.Errorf("honeycomb: %w", err)
	}
	return endpoint(a.o.Endpoint)
}

func (a honeycombAction) Render(data any, funcs template.FuncMap) (Step, error) {
	b := a
	if err := render(data, funcs,
		&b.o.Dataset, &b.o.Message, &b.o.Type, &b.o.URL, &b.o.Endpoint); err != nil {
		return Step{}, err
	}
	return Step{line: b.Describe(), run: b.run}, nil
}

func (a honeycombAction) run(ctx context.Context, _ *Exec) error {
	return announce(a.Describe(), a.mark(ctx))
}

func (a honeycombAction) mark(ctx context.Context) error {
	key, err := token(a.o.KeyEnv)
	if err != nil {
		return err
	}

	body := map[string]string{"type": a.o.Type, "message": a.o.Message}
	if a.o.URL != "" {
		body["url"] = a.o.URL
	}

	// No start_time: Honeycomb stamps it on arrival, which is the moment the
	// deploy finished and the only moment worth marking.
	url := fmt.Sprintf("%s/1/markers/%s",
		strings.TrimSuffix(a.o.Endpoint, "/"), a.o.Dataset)

	status, said, err := postJSON(ctx, url, map[string]string{"X-Honeycomb-Team": key}, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return refused(status, said)
	}
	return nil
}
