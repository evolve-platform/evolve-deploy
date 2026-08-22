package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// announceTimeout bounds one call. An annotation service that has stopped
// answering must not hold a release open: the release is the point, and the
// marker is a note in the margin.
const announceTimeout = 30 * time.Second

// snippet is how much of a failed response is worth repeating. Enough for the
// message an API puts in its JSON, not enough to bury the release under a
// page of HTML from a proxy.
const snippet = 1 << 10

// announce reports that a call which only records something did not happen,
// without failing the release. See noted for why that is the right way round.
//
// The failure that is actually common, a token nobody set, is refused during
// planning instead. See Checker.
func announce(what string, err error) error {
	if err == nil {
		return nil
	}
	return Noted(fmt.Errorf("%s not recorded: %w", what, err))
}

// token reads an API token out of the environment.
//
// Out of the environment and not out of the config: it is a secret, deploy
// configs are in the repository, and a pipeline already has somewhere to put
// one.
func token(env string) (string, error) {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return "", fmt.Errorf("%s is not set in the environment", env)
	}
	return v, nil
}

// endpoint checks that a base URL is one, so a typo in it is a refusal during
// planning rather than a marker that silently goes nowhere.
func endpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("endpoint: %q is not a url: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint: %q needs a scheme, as in https://api.honeycomb.io", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint: %q names no host", raw)
	}
	return nil
}

// postJSON makes one call and returns what came back, so the caller decides
// which statuses it considers a success — "this release already exists" is one
// for Sentry and a failure for nobody else.
func postJSON(ctx context.Context, url string, header map[string]string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, announceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	read, _ := io.ReadAll(io.LimitReader(resp.Body, snippet))
	return resp.StatusCode, read, nil
}

// refused turns a response into the error a reader can act on: the status, and
// whatever the API said about it.
func refused(status int, body []byte) error {
	said := strings.TrimSpace(string(body))
	if said == "" {
		return fmt.Errorf("%d %s", status, http.StatusText(status))
	}
	return fmt.Errorf("%d %s: %s", status, http.StatusText(status), oneLine(said))
}

// oneLine flattens a response body, which is usually JSON printed over several
// lines, so it cannot break the column a hook's output is tagged into.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
