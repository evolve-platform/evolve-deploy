package target

import (
	"strings"
	"testing"
)

func TestPickContainer(t *testing.T) {
	tests := []struct {
		name         string
		containers   []string
		configured   string
		conventional string
		want         string
		wantErr      string
	}{
		{
			name:         "a single container needs no configuration",
			containers:   []string{"whatever"},
			conventional: "app",
			want:         "whatever",
		},
		{
			// The common shape in this platform: an application container next
			// to a reverse proxy, and on some services an OpenTelemetry
			// collector as well.
			name:         "application plus sidecar falls back to the convention",
			containers:   []string{"app", "reverse-proxy"},
			conventional: "app",
			want:         "app",
		},
		{
			name:         "an explicit name wins",
			containers:   []string{"main", "reverse-proxy"},
			configured:   "reverse-proxy",
			conventional: "main",
			want:         "reverse-proxy",
		},
		{
			name:         "an explicit name that is not there is an error",
			containers:   []string{"app"},
			configured:   "nope",
			conventional: "app",
			wantErr:      "no container named",
		},
		{
			// Guessing here would mean retagging a proxy image with the
			// application's version.
			name:         "guessing between several is refused",
			containers:   []string{"one", "two"},
			conventional: "app",
			wantErr:      "cannot tell which",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PickContainer(tc.containers, tc.configured, tc.conventional)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got (%q, %v), want an error mentioning %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
