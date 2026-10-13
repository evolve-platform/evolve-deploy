package gcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"google.golang.org/api/option"
	parametermanager "google.golang.org/api/parametermanager/v1"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

func TestPinCapturesParameterAndSecretMetadata(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v1/projects/test/locations/global/parameters/purchase/versions/latest", "/v1/projects/test/locations/global/parameters/purchase/versions/config-a":
			if req.URL.Query().Get("view") != "BASIC" {
				t.Error("requested parameter payload")
			}
			fmt.Fprint(w, `{"name":"projects/test/locations/global/parameters/purchase/versions/config-a"}`)
		case "/v1/projects/test/locations/europe-west4/parameters/purchase/versions/config-a":
			fmt.Fprint(w, `{"name":"projects/test/locations/europe-west4/parameters/purchase/versions/config-a"}`)
		case "/v1/projects/test/locations/global/parameters/purchase/versions/disabled":
			fmt.Fprint(w, `{"name":"projects/test/locations/global/parameters/purchase/versions/disabled","disabled":true}`)
		case "/v1/projects/test/locations/global/parameters/purchase/versions/unresolved":
			fmt.Fprint(w, `{"name":"projects/test/locations/global/parameters/purchase/versions/latest"}`)
		case "/v1/projects/test/secrets/token/versions/latest", "/v1/projects/test/secrets/token/versions/7":
			fmt.Fprint(w, `{"name":"projects/test/secrets/token/versions/7","state":"ENABLED"}`)
		case "/v1/projects/test/secrets/token/versions/8":
			fmt.Fprint(w, `{"name":"projects/test/secrets/token/versions/8","state":"DISABLED"}`)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()
	pm, err := parametermanager.NewService(ctx, option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	sm, err := secretmanager.NewRESTClient(ctx, option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sm.Close(); err != nil {
			t.Error(err)
		}
	})
	r := &resolver{d: &Driver{file: &config.File{Cloud: config.CloudConfig{Project: "test"}}, parameters: pm, secrets: sm}}
	for _, name := range []string{"purchase", "purchase/versions/latest", "purchase/versions/config-a", "projects/test/locations/europe-west4/parameters/purchase/versions/config-a"} {
		got, err := r.Pin(ctx, refs.Value{Kind: refs.ParameterVersion, Name: name})
		if err != nil || got.Kind != refs.Literal || got.Literal != "config-a" {
			t.Fatalf("%s: %+v, %v", name, got, err)
		}
	}
	for _, name := range []string{"purchase/versions/disabled", "purchase/versions/unresolved", "missing", "purchase/versions/", "purchase/versions/a/b"} {
		if _, err := r.Pin(ctx, refs.Value{Kind: refs.ParameterVersion, Name: name}); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	for _, name := range []string{"token", "token/versions/latest", "token/versions/7", "projects/test/secrets/token/versions/7"} {
		got, err := r.Pin(ctx, refs.Value{Kind: refs.Secret, Name: name})
		if err != nil || got.Kind != refs.Secret || !strings.HasSuffix(got.Name, "/versions/7") {
			t.Fatalf("%s: %+v, %v", name, got, err)
		}
		rendered := renderEnv([]target.EnvVar{{Name: "TOKEN", Value: got}})[0].GetValueSource().GetSecretKeyRef()
		if rendered.GetVersion() != "7" || strings.Contains(rendered.GetSecret(), "/versions/") {
			t.Fatalf("bad native reference: %v", rendered)
		}
	}
	for _, name := range []string{"token/versions/8", "missing", "token/versions/nope"} {
		if _, err := r.Pin(ctx, refs.Value{Kind: refs.Secret, Name: name}); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestVersionOnlyChangesStageANewEnvironment(t *testing.T) {
	old := []*runpb.Container{{Name: "app", Env: renderEnv([]target.EnvVar{
		{Name: "PARAMETER_MANAGER_VERSION", Value: refs.Value{Literal: "config-a"}},
		{Name: "TOKEN", Value: refs.Value{Kind: refs.Secret, Name: "token/versions/7"}},
	})}}
	next := []*runpb.Container{{Name: "app", Env: renderEnv([]target.EnvVar{
		{Name: "PARAMETER_MANAGER_VERSION", Value: refs.Value{Literal: "config-b"}},
		{Name: "TOKEN", Value: refs.Value{Kind: refs.Secret, Name: "token/versions/8"}},
	})}}
	_, changed, _ := diffEnv(old, next, "app", nil)
	if fmt.Sprint(changed) != "[PARAMETER_MANAGER_VERSION TOKEN]" {
		t.Fatal(changed)
	}
}
