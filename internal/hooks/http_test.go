package hooks

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPPassesOnA2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	out, err := runHook(t, fmt.Sprintf(`{uses: http, with: {url: "%s/health"}}`, srv.URL), deploy())
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("a check that passed printed %q", out)
	}
}

func TestHTTPFailsOnAnythingElse(t *testing.T) {
	// Leaving --fail off a curl was the way to write a smoke test that a 500
	// walked straight through. There is no way to leave it off here.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "subgraph purchase unreachable")
	}))
	defer srv.Close()

	_, err := runHook(t, fmt.Sprintf(`{uses: http, with: {url: "%s/health"}}`, srv.URL), deploy())
	if err == nil {
		t.Fatal("a 500 passed the check")
	}
	// The body, because a health route that is failing says why in it.
	if !strings.Contains(err.Error(), "subgraph purchase unreachable") {
		t.Errorf("error was %q", err)
	}
}

func TestHTTPRetriesAndThenGivesUp(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := runHook(t, fmt.Sprintf(
		`{uses: http, with: {url: "%s/health", retry: 2, delay: 1ms}}`, srv.URL), deploy())
	if err == nil {
		t.Fatal("a check that never passed was reported as passing")
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("tried %d times, want 3 — retry counts the attempts after the first", got)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error was %q", err)
	}
}

func TestHTTPRetryCoversARevisionThatIsNotAnsweringYet(t *testing.T) {
	// The case it exists for: a side that has staged is not always serving the
	// instant staging returns, and the first refused request of a smoke test is
	// nearly always that rather than a broken deploy.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := runHook(t, fmt.Sprintf(
		`{uses: http, with: {url: "%s/health", retry: 5, delay: 1ms}}`, srv.URL), deploy()); err != nil {
		t.Fatalf("gave up on a revision that came up on the third try: %v", err)
	}
}

func TestHTTPSendsWhatItWasGiven(t *testing.T) {
	var (
		method, auth, body, path string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, _ := io.ReadAll(r.Body)
		method, auth, body, path = r.Method, r.Header.Get("Authorization"), string(read), r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	_, err := runHook(t, fmt.Sprintf(`
uses: http
with:
  url: "%s/graphql"
  method: post
  status: 202
  headers: { Authorization: "Bearer {{.version}}" }
  body: '{"query":"{ health }"}'
`, srv.URL), deploy())
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/graphql" {
		t.Errorf("%s %s", method, path)
	}
	if auth != "Bearer abc1234" {
		t.Errorf("headers are not rendered: %q", auth)
	}
	if body != `{"query":"{ health }"}` {
		t.Errorf("body = %q", body)
	}
}

func TestHTTPHoldsOutForTheStatusItWasAsked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := runHook(t, fmt.Sprintf(
		`{uses: http, with: {url: "%s/health", status: 204}}`, srv.URL), deploy())
	if err == nil {
		t.Fatal("a 200 satisfied a check that asked for a 204")
	}
}
