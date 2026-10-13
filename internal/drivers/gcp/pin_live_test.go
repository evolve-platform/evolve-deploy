//go:build integration

package gcp

import (
	"context"
	"os"
	"testing"
	"time"

	parametermanager "google.golang.org/api/parametermanager/v1"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
)

// This catches differences between the live metadata and render APIs that an
// HTTP fixture cannot establish. It reads version metadata only and never writes.
func TestPinParameterVersionLive(t *testing.T) {
	project := os.Getenv("EVOLVE_GCP_PIN_PROJECT")
	parameter := os.Getenv("EVOLVE_GCP_PIN_PARAMETER")
	if project == "" || parameter == "" {
		t.Skip("set EVOLVE_GCP_PIN_PROJECT and EVOLVE_GCP_PIN_PARAMETER to an existing global parameter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pm, err := parametermanager.NewService(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := &resolver{d: &Driver{file: &config.File{Cloud: config.CloudConfig{Project: project}}, parameters: pm}}
	got, err := r.Pin(ctx, refs.Value{Kind: refs.ParameterVersion, Name: parameter})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != refs.Literal || got.Literal == "" || got.Literal == "latest" {
		t.Fatalf("expected a concrete version, got %+v", got)
	}
	explicit, err := r.Pin(ctx, refs.Value{Kind: refs.ParameterVersion, Name: parameter + "/versions/" + got.Literal})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Literal != got.Literal {
		t.Fatalf("explicit version %q differs from captured version %q", explicit.Literal, got.Literal)
	}
	t.Logf("pinned %s to %s", parameter, got.Literal)
}
