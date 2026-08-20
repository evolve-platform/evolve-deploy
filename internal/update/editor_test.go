package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T, body string) (*Editor, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "acc.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return e, path
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The whole reason this is a line edit and not a re-marshal: a comment, a flow
// mapping and the key order all have to survive, because the diff is what gets
// reviewed.
func TestSetChangesNothingButTheVersion(t *testing.T) {
	body := `cloud:
  provider: azure
  subscription: "0000"
  resource_group: evolve-acc   # from terraform

services:
  discover:
    version: abc1234           # promoted from tst
    envFrom:
      - ${param:/evolve/${env}/discover/setup}
    targets:
      - { type: container-app,     name: discover }
      - { type: container-app-job, name: discover-products }

  site:
    version: '9999999'
    type: container-app
`

	e, path := open(t, body)
	if err := e.Set("discover", "9f8e7d6"); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(); err != nil {
		t.Fatal(err)
	}

	want := strings.Replace(body,
		"version: abc1234           # promoted from tst",
		"version: 9f8e7d6           # promoted from tst", 1)
	if got := read(t, path); got != want {
		t.Errorf("file is\n%s\nwant\n%s", got, want)
	}
}

// A quoted version stays quoted. A version that looks like a number — every
// all-digit sha — has to keep its quotes or it stops being a string.
func TestSetKeepsQuoting(t *testing.T) {
	body := `services:
  site:
    version: '9999999'
    type: ecs
    cluster: platform
`

	e, path := open(t, body)
	if err := e.Set("site", "1234567"); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(); err != nil {
		t.Fatal(err)
	}

	if got := read(t, path); !strings.Contains(got, "version: '1234567'") {
		t.Errorf("file is\n%s\nwant a single-quoted version", got)
	}
}

func TestSetOnAServiceInAFlowMapping(t *testing.T) {
	body := `services:
  site: { version: abc1234, type: ecs, cluster: platform }
`

	e, path := open(t, body)
	if err := e.Set("site", "9f8e7d6"); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(); err != nil {
		t.Fatal(err)
	}

	want := "  site: { version: 9f8e7d6, type: ecs, cluster: platform }\n"
	if got := read(t, path); !strings.Contains(got, want) {
		t.Errorf("file is\n%s\nwant it to contain\n%s", got, want)
	}
}

// Save must not add or remove a trailing newline: that is a line in every diff
// of the file, forever.
func TestSaveLeavesTheEndOfTheFileAlone(t *testing.T) {
	for _, body := range []string{
		"services:\n  site:\n    version: abc1234\n    type: ecs\n",
		"services:\n  site:\n    version: abc1234\n    type: ecs",
	} {
		e, path := open(t, body)
		if err := e.Set("site", "9f8e7d6"); err != nil {
			t.Fatal(err)
		}
		if err := e.Save(); err != nil {
			t.Fatal(err)
		}

		got := read(t, path)
		if strings.HasSuffix(body, "\n") != strings.HasSuffix(got, "\n") {
			t.Errorf("trailing newline changed: %q became %q", body, got)
		}
	}
}

func TestLineIsWhereTheVersionIs(t *testing.T) {
	e, _ := open(t, `services:
  site:
    version: abc1234
    type: ecs
  discover:
    version: abc1234
    type: container-app
`)

	if got := e.Line("site"); got != 3 {
		t.Errorf("Line(site) = %d, want 3", got)
	}
	if got := e.Line("discover"); got != 6 {
		t.Errorf("Line(discover) = %d, want 6", got)
	}
	// An unknown service has no line rather than an error: Line only orders the
	// questions.
	if got := e.Line("nope"); got != 0 {
		t.Errorf("Line(nope) = %d, want 0", got)
	}
}

func TestSetRefusesWhenTheFileMovedUnderIt(t *testing.T) {
	e, path := open(t, "services:\n  site:\n    version: abc1234\n    type: ecs\n")

	// Someone editing the file between reading and writing is the case this
	// guard exists for: the recorded position no longer holds the version, and
	// writing there would land in the middle of something else.
	if err := os.WriteFile(path, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.lines = []string{"services:", "  site:", "    other: not-a-version-here", ""}

	err := e.Set("site", "9f8e7d6")
	if err == nil {
		t.Fatal("want an error when the version is not where it was")
	}
	if !strings.Contains(err.Error(), "edit this file by hand") {
		t.Errorf("error = %q, want it to say what to do instead", err)
	}
}

func TestSetOnAnUnknownService(t *testing.T) {
	e, _ := open(t, "services:\n  site:\n    version: abc1234\n    type: ecs\n")

	if err := e.Set("discover", "9f8e7d6"); err == nil {
		t.Fatal("want an error for a service that is not in the file")
	}
}
