package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type sentryCall struct {
	path string
	auth string
	body map[string]any
}

func sentryServer(t *testing.T, status func(path string) int) (*httptest.Server, *[]sentryCall) {
	t.Helper()
	var calls []sentryCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := sentryCall{path: r.URL.Path, auth: r.Header.Get("Authorization")}
		read, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(read, &c.body)
		calls = append(calls, c)
		w.WriteHeader(status(r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestSentryRegistersTheReleaseThenTheDeploy(t *testing.T) {
	t.Setenv("SENTRY_AUTH_TOKEN", "secret")
	srv, calls := sentryServer(t, func(string) int { return http.StatusCreated })

	out, err := runHook(t, fmt.Sprintf(
		`{uses: sentry, with: {org: acme, endpoint: %s, repository: acme/shop, commit: "{{.version}}"}}`,
		srv.URL), deploy())
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("a release that registered printed %q", out)
	}
	if len(*calls) != 2 {
		t.Fatalf("made %d calls, want 2", len(*calls))
	}

	release, deployed := (*calls)[0], (*calls)[1]
	if release.path != "/organizations/acme/releases/" {
		t.Errorf("release path = %q", release.path)
	}
	if release.auth != "Bearer secret" {
		t.Errorf("token was not sent, header = %q", release.auth)
	}
	if release.body["version"] != "abc1234" {
		t.Errorf("release body = %v", release.body)
	}
	// The project defaults to the service, which is the case that needs no
	// configuration at all.
	if projects, ok := release.body["projects"].([]any); !ok ||
		len(projects) != 1 || projects[0] != "purchase" {
		t.Errorf("projects = %v", release.body["projects"])
	}
	if refs, ok := release.body["refs"].([]any); !ok || len(refs) != 1 {
		t.Errorf("refs = %v", release.body["refs"])
	}

	// The second call is the one that differs between tst and prd: the same
	// release, arriving somewhere.
	if deployed.path != "/organizations/acme/releases/abc1234/deploys/" {
		t.Errorf("deploy path = %q", deployed.path)
	}
	if deployed.body["environment"] != "tst" {
		t.Errorf("deploy body = %v", deployed.body)
	}
}

func TestAReleaseThatAlreadyExistsIsStillDeployed(t *testing.T) {
	// Two services in one release both report the same version. The second one
	// finds it already there, which is the normal case and not a failure — and
	// its deploy still has to be recorded.
	t.Setenv("SENTRY_AUTH_TOKEN", "secret")
	srv, calls := sentryServer(t, func(path string) int {
		if strings.HasSuffix(path, "/releases/") {
			return http.StatusAlreadyReported
		}
		return http.StatusCreated
	})

	out, err := runHook(t, fmt.Sprintf(
		`{uses: sentry, with: {org: acme, endpoint: %s}}`, srv.URL), deploy())
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("an existing release was reported as a problem: %q", out)
	}
	if len(*calls) != 2 {
		t.Fatalf("made %d calls, want 2", len(*calls))
	}
}

func TestSentryRefusalIsReportedAndForgiven(t *testing.T) {
	t.Setenv("SENTRY_AUTH_TOKEN", "secret")
	srv, _ := sentryServer(t, func(string) int { return http.StatusForbidden })

	out, err := runHook(t, fmt.Sprintf(
		`{uses: sentry, with: {org: acme, endpoint: %s}}`, srv.URL), deploy())
	if err != nil {
		t.Fatalf("a missing release failed the deploy: %v", err)
	}
	if !strings.Contains(out, "not recorded") || !strings.Contains(out, "403") {
		t.Errorf("output was %q", out)
	}
}
