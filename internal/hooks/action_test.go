package hooks

import (
	"bytes"
	"context"
	"testing"

	"gopkg.in/yaml.v3"
)

// runHook renders and runs one entry the way a release would, and returns
// everything it printed.
func runHook(t *testing.T, doc string, vars Vars) (string, error) {
	t.Helper()
	var h Hook
	if err := yaml.Unmarshal([]byte(doc), &h); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Action(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	r := &Runner{Out: &out}
	err := r.Run(context.Background(), "purchase", "after", []*Hook{&h}, vars)
	return out.String(), err
}

// deploy is the service a hook is normally given.
func deploy() Vars {
	return Vars{"name": "purchase", "version": "abc1234", "env": "tst"}
}
