package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHoneycombMarksTheDeploy(t *testing.T) {
	t.Setenv("HONEYCOMB_API_KEY", "secret")

	var (
		path, team string
		body       map[string]string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, team = r.URL.Path, r.Header.Get("X-Honeycomb-Team")
		read, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(read, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	out, err := runHook(t, fmt.Sprintf(
		`{uses: honeycomb, with: {dataset: purchase, endpoint: %s, url: "https://git.test/{{.version}}"}}`,
		srv.URL), deploy())
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("a marker that worked printed %q", out)
	}
	if path != "/1/markers/purchase" {
		t.Errorf("path = %q", path)
	}
	if team != "secret" {
		t.Errorf("key was not sent, header = %q", team)
	}
	if body["message"] != "purchase abc1234" || body["type"] != "deploy" ||
		body["url"] != "https://git.test/abc1234" {
		t.Errorf("body = %v", body)
	}
}

func TestAMarkerThatFailsDoesNotFailTheRelease(t *testing.T) {
	// The reason this is worth having as an action at all. Written as curl it
	// was `|| echo "marker not recorded"` on the end of every line, and one
	// line without it turned a good deploy into a red pipeline.
	t.Setenv("HONEYCOMB_API_KEY", "secret")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"dataset not found"}`)
	}))
	defer srv.Close()

	out, err := runHook(t, fmt.Sprintf(
		`{uses: honeycomb, with: {dataset: purchase, endpoint: %s}}`, srv.URL), deploy())
	if err != nil {
		t.Fatalf("a missing marker failed the release: %v", err)
	}
	// But it does have to say so. Nothing else is left behind to notice it by.
	for _, want := range []string{"not recorded", "500", "dataset not found"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not mention %q", out, want)
		}
	}
}

func TestAMissingKeyIsRefusedBeforeAnythingIsDeployed(t *testing.T) {
	// The failure that actually happens: a pipeline that gained a second
	// environment and only gave the first one its secret. Found during
	// planning it costs nothing; found from an `after` hook it is a red run on
	// a release that already worked.
	t.Setenv("HONEYCOMB_API_KEY", "")

	var h Hook
	if err := yaml.Unmarshal([]byte(`{uses: honeycomb, with: {dataset: purchase}}`), &h); err != nil {
		t.Fatal(err)
	}
	err := Validate([]*Hook{&h}, deploy())
	if err == nil {
		t.Fatal("a marker with no key anywhere was accepted")
	}
	if !strings.Contains(err.Error(), "HONEYCOMB_API_KEY is not set") {
		t.Errorf("error was %q, and has to name the variable", err)
	}
}
