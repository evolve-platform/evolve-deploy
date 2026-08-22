package hooks

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

type httpOptions struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	// Status is the one status this counts as healthy. Empty means any 2xx,
	// which is what a health route means by working.
	Status int `yaml:"status"`
	// Timeout bounds one attempt, not the lot of them.
	Timeout string `yaml:"timeout"`
	// Retry is how many further attempts a failure gets. A revision that has
	// staged is not always answering yet, and the first refused connection of
	// a smoke test is nearly always that rather than a broken deploy.
	Retry int    `yaml:"retry"`
	Delay string `yaml:"delay"`
}

// httpAction asks for one url and says whether the answer was the expected one.
//
// It replaces the row of curl flags this used to be written as — --fail
// --silent --show-error --max-time --retry --retry-delay --retry-connrefused —
// where every one of them had to be remembered and leaving out --fail meant a
// 500 passed the smoke test.
type httpAction struct {
	o       httpOptions
	timeout time.Duration
	delay   time.Duration
}

func parseHTTP(with *yaml.Node) (Action, error) {
	var o httpOptions
	if err := decode(with, &o); err != nil {
		return nil, err
	}
	if o.URL == "" {
		return nil, fmt.Errorf("http: `url` is required")
	}
	if o.Status != 0 && (o.Status < 100 || o.Status > 599) {
		return nil, fmt.Errorf("http: status: %d is not a status code", o.Status)
	}
	if o.Retry < 0 {
		return nil, fmt.Errorf("http: retry: %d is negative", o.Retry)
	}
	o.Method = strings.ToUpper(cmp.Or(o.Method, http.MethodGet))

	a := httpAction{o: o, timeout: 10 * time.Second, delay: 3 * time.Second}
	for _, d := range []struct {
		name string
		raw  string
		into *time.Duration
	}{
		{"timeout", o.Timeout, &a.timeout},
		{"delay", o.Delay, &a.delay},
	} {
		if d.raw == "" {
			continue
		}
		v, err := time.ParseDuration(d.raw)
		if err != nil {
			return nil, fmt.Errorf("http: %s: %q is not a duration (try 10s or 1m)", d.name, d.raw)
		}
		if v < 0 {
			return nil, fmt.Errorf("http: %s: %s is negative", d.name, v)
		}
		*d.into = v
	}
	return a, nil
}

func (a httpAction) Describe() string {
	out := fmt.Sprintf("http %s %s", a.o.Method, a.o.URL)
	if a.o.Status != 0 {
		out += fmt.Sprintf(", expecting %d", a.o.Status)
	}
	if a.o.Retry > 0 {
		out += fmt.Sprintf(", retrying %d times", a.o.Retry)
	}
	return out
}

func (a httpAction) Render(data any, funcs template.FuncMap) (Step, error) {
	b := a
	b.o.Headers = maps.Clone(a.o.Headers)
	fields := []*string{&b.o.URL, &b.o.Body}
	for k := range b.o.Headers {
		v := b.o.Headers[k]
		if err := render(data, funcs, &v); err != nil {
			return Step{}, err
		}
		b.o.Headers[k] = v
	}
	if err := render(data, funcs, fields...); err != nil {
		return Step{}, err
	}
	return Step{line: b.Describe(), run: b.run}, nil
}

// run tries until it runs out of attempts, and reports the last failure rather
// than all of them: a check that failed six times failed for the same reason
// six times, and the sixth is the one that was still true when it gave up.
func (a httpAction) run(ctx context.Context, e *Exec) error {
	started := time.Now()
	var err error
	for attempt := 0; attempt <= a.o.Retry; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(a.delay):
			}
		}
		if err = a.try(ctx); err == nil {
			return nil
		}
	}

	if a.o.Retry == 0 {
		return err
	}
	return fmt.Errorf("%w, after %d attempts in %s",
		err, a.o.Retry+1, time.Since(started).Round(time.Second))
}

func (a httpAction) try(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var body io.Reader
	if a.o.Body != "" {
		body = strings.NewReader(a.o.Body)
	}
	req, err := http.NewRequestWithContext(ctx, a.o.Method, a.o.URL, body)
	if err != nil {
		return err
	}
	for k, v := range a.o.Headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	ok := resp.StatusCode/100 == 2
	if a.o.Status != 0 {
		ok = resp.StatusCode == a.o.Status
	}
	if !ok {
		// With the body, because a health route that is failing says why in it,
		// and that sentence is the whole reason to look at the response rather
		// than only its number.
		read, _ := io.ReadAll(io.LimitReader(resp.Body, snippet))
		return refused(resp.StatusCode, read)
	}
	return nil
}
