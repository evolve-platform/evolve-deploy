package hooks

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hook reads one entry the way a config file writes it.
func hook(t *testing.T, doc string) (Action, error) {
	t.Helper()
	var h Hook
	if err := yaml.Unmarshal([]byte(doc), &h); err != nil {
		t.Fatalf("a hook entry must never fail the decode: %v", err)
	}
	return h.Action()
}

func TestAPlainStringIsStillACommand(t *testing.T) {
	// The form every existing config is made of. It has to keep working
	// unchanged, and it has to mean exactly what it meant before.
	a, err := hook(t, `hive schema:check --commit {{.version}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.Describe(), "hive schema:check --commit {{.version}}"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}

func TestABlockScalarPrintsAsOneLine(t *testing.T) {
	a, err := hook(t, ">-\n  curl --fail\n  --retry 5\n  https://example.test/health")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.Describe(), "curl --fail --retry 5 https://example.test/health"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}

func TestCmdIsTheSameThingWrittenOut(t *testing.T) {
	a, err := hook(t, `{cmd: "echo hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.(cmdAction); !ok {
		t.Fatalf("cmd parsed as %T", a)
	}
}

func TestUsesNamesAnAction(t *testing.T) {
	a, err := hook(t, `{uses: honeycomb, with: {dataset: purchase}}`)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := a.(honeycombAction)
	if !ok {
		t.Fatalf("honeycomb parsed as %T", a)
	}
	// The defaults are half the point of the action existing: everything in
	// them was a flag on a curl line before.
	if h.o.Type != "deploy" || h.o.Endpoint != honeycombDefault ||
		h.o.KeyEnv != "HONEYCOMB_API_KEY" || h.o.Message != "{{.name}} {{.version}}" {
		t.Errorf("defaults are %+v", h.o)
	}
}

func TestRefusedEntries(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"both forms", `{cmd: "echo x", uses: http}`,
			"set either `cmd` or `uses`, not both"},
		{"neither form", `{}`,
			"needs either `cmd` (a command line) or `uses` (a named action)"},
		{"with on a cmd", `{cmd: "echo x", with: {url: y}}`,
			"`with` names the options of a `uses`"},
		{"misspelt field", `{use: http}`,
			`"use" is not a hook field`},
		{"unknown action", `{uses: honycomb, with: {dataset: x}}`,
			`uses: "honycomb" is not one of honeycomb, http, sentry`},
		{"cmd is not an action", `{uses: cmd}`,
			`uses: "cmd" is not one of`},
		{"a list is not a hook", `[a, b]`,
			"a hook is either a command line or a `uses` block"},
		{"with is not a list", `{uses: http, with: [a]}`,
			"`with` is a block of options"},
		{"unknown option", `{uses: honeycomb, with: {datasett: purchase}}`,
			"no such option: datasett"},
		{"missing required option", `{uses: honeycomb}`,
			"honeycomb: `dataset` is required"},
		{"http needs a url", `{uses: http, with: {retry: 3}}`,
			"http: `url` is required"},
		{"not a duration", `{uses: http, with: {url: x, timeout: soon}}`,
			`http: timeout: "soon" is not a duration`},
		{"not a status", `{uses: http, with: {url: x, status: 20}}`,
			"http: status: 20 is not a status code"},
		{"sentry needs an org", `{uses: sentry}`,
			"sentry: `org` is required"},
		{"both project spellings", `{uses: sentry, with: {org: o, project: p, projects: [q]}}`,
			"set either `project` or `projects`, not both"},
		{"a commit with no repository", `{uses: sentry, with: {org: o, commit: abc1234}}`,
			"needs a `repository`"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := hook(t, tt.body)
			if err == nil {
				t.Fatalf("%s was accepted", tt.body)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error was %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestCheckNamesTheEntryThatIsWrong(t *testing.T) {
	// The whole reason a bad entry does not fail the decode: one of four hooks
	// is wrong and the message has to say which one.
	var hs []*Hook
	if err := yaml.Unmarshal([]byte(`
- echo one
- echo two
- {uses: honeycomb}
`), &hs); err != nil {
		t.Fatal(err)
	}

	msgs := Check("services.purchase.after", hs)
	if len(msgs) != 1 {
		t.Fatalf("messages = %q", msgs)
	}
	if !strings.HasPrefix(msgs[0], "services.purchase.after[2]: ") {
		t.Errorf("message does not name the entry: %q", msgs[0])
	}
}
