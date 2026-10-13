package gcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"google.golang.org/api/option"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
)

func TestPinnedReferencesUseExactVersionAndMetadataOnlyVerification(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		paths = append(paths, req.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/versions/7:access"):
			fmt.Fprintf(w, `{"payload":{"data":%q}}`, base64.StdEncoding.EncodeToString([]byte(`{"MODE":"old"}`)))
		case strings.HasSuffix(req.URL.Path, "/versions/8"):
			fmt.Fprint(w, `{"state":"DISABLED"}`)
		case strings.HasSuffix(req.URL.Path, "/versions/7"):
			fmt.Fprint(w, `{"state":"ENABLED"}`)
		default:
			http.Error(w, "unexpected secret request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	client, err := secretmanager.NewRESTClient(ctx, option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	r := &resolver{d: &Driver{file: &config.File{Cloud: config.CloudConfig{Project: "test"}}, secrets: client}}
	ref := refs.Value{Kind: refs.Secret, Name: "token/versions/7"}
	if err := r.Verify(ctx, ref); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	verified := append([]string{}, paths...)
	mu.Unlock()
	if len(verified) != 1 || verified[0] != "/v1/projects/test/secrets/token/versions/7" {
		t.Fatalf("verification accessed payload or wrong version: %v", verified)
	}
	values, err := r.ReadMap(ctx, ref)
	if err != nil || values["MODE"] != "old" {
		t.Fatalf("ReadMap = %v, %v", values, err)
	}
	ref.Name = "token/versions/8"
	if err := r.Verify(ctx, ref); err == nil || !strings.Contains(err.Error(), "DISABLED") {
		t.Fatalf("disabled version accepted: %v", err)
	}
}
