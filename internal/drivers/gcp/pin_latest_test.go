package gcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/api/option"
	parametermanager "google.golang.org/api/parametermanager/v1"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
)

func TestPinLatestParameterVersion(t *testing.T) {
	const prefix = "projects/test/locations/global/parameters/purchase/versions/"
	for _, tt := range []struct {
		name  string
		pages []string
		want  string
		err   string
	}{
		{
			name: "all pages by creation time rather than ID, update time or response order",
			pages: []string{
				`{"parameterVersions":[{"name":"` + prefix + `zzz-old","createTime":"2026-10-13T13:00:00Z","updateTime":"2026-10-14T00:00:00Z"}],"nextPageToken":"1"}`,
				`{"parameterVersions":[{"name":"` + prefix + `aaa-new","createTime":"2026-10-13T13:00:00.000000001Z"},{"name":"` + prefix + `offset-old","createTime":"2026-10-13T14:00:00+02:00"}],"nextPageToken":"2"}`,
				`{"parameterVersions":[{"name":"` + prefix + `last-old","createTime":"2026-10-13T12:00:00Z"}]}`,
			},
			want: "aaa-new",
		},
		{
			name: "empty page with continuation",
			pages: []string{
				`{"nextPageToken":"1"}`,
				`{"parameterVersions":[{"name":"` + prefix + `config-a","createTime":"2026-10-13T13:00:00Z"}]}`,
			},
			want: "config-a",
		},
		{
			name:  "disabled newest does not silently fall back",
			pages: []string{`{"parameterVersions":[{"name":"` + prefix + `old","createTime":"2026-10-13T12:00:00Z"},{"name":"` + prefix + `new","createTime":"2026-10-13T13:00:00Z","disabled":true}]}`},
			err:   "is disabled",
		},
		{
			name: "no versions", pages: []string{`{}`}, err: "no parameter versions exist",
		},
		{
			name:  "missing creation time",
			pages: []string{`{"parameterVersions":[{"name":"` + prefix + `config-a"}]}`},
			err:   "invalid creation time",
		},
		{
			name: "incomplete response", pages: []string{`{"unreachable":["global"]}`}, err: "locations unreachable",
		},
		{
			name:  "ambiguous newest versions",
			pages: []string{`{"parameterVersions":[{"name":"` + prefix + `a","createTime":"2026-10-13T13:00:00Z"},{"name":"` + prefix + `b","createTime":"2026-10-13T13:00:00Z"}]}`},
			err:   "select an explicit",
		},
		{
			name: "permission failure", pages: []string{`{"error":{"code":403,"message":"permission denied"}}`}, err: "permission denied",
		},
		{
			name: "later page failure does not use a partial result",
			pages: []string{
				`{"parameterVersions":[{"name":"` + prefix + `old","createTime":"2026-10-13T13:00:00Z"}],"nextPageToken":"1"}`,
				`{"error":{"code":403,"message":"permission denied"}}`,
			},
			err: "permission denied",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if req.URL.Path != "/v1/projects/test/locations/global/parameters/purchase/versions" {
					t.Errorf("unexpected API request: %s", req.URL.Path)
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				if req.URL.Query().Get("fields") != "parameterVersions(name,createTime,disabled),nextPageToken,unreachable" {
					t.Error("request must contain metadata only")
				}
				page := 0
				if token := req.URL.Query().Get("pageToken"); token != "" {
					var err error
					page, err = strconv.Atoi(token)
					if err != nil {
						t.Error(err)
					}
				}
				requests++
				if page < 0 || page >= len(tt.pages) {
					t.Errorf("unexpected page %d", page)
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				if strings.Contains(tt.pages[page], `"error"`) {
					w.WriteHeader(http.StatusForbidden)
				}
				fmt.Fprint(w, tt.pages[page])
			}))
			defer server.Close()
			ctx := context.Background()
			pm, err := parametermanager.NewService(ctx, option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
			if err != nil {
				t.Fatal(err)
			}
			r := &resolver{d: &Driver{file: &config.File{Cloud: config.CloudConfig{Project: "test"}}, parameters: pm}}
			got, err := r.Pin(ctx, refs.Value{Kind: refs.ParameterVersion, Name: "purchase"})
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("got %v, want error containing %q", err, tt.err)
				}
				return
			}
			if err != nil || got.Kind != refs.Literal || got.Literal != tt.want {
				t.Fatalf("got %+v, %v; want %s", got, err, tt.want)
			}
			if requests != len(tt.pages) {
				t.Fatalf("read %d pages, want %d", requests, len(tt.pages))
			}
		})
	}
}
