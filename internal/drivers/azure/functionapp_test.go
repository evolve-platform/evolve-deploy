package azure

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appservice/armappservice/v5"
)

func TestDetectMode(t *testing.T) {
	// Which of the three deployment styles applies is a property of the app,
	// not something the config should have to repeat — so it is read off the
	// resource.
	tests := []struct {
		name  string
		props *armappservice.SiteProperties
		cfg   *armappservice.SiteConfig
		want  funcMode
	}{
		{
			name: "an image means a tag change, like any other container",
			cfg:  &armappservice.SiteConfig{LinuxFxVersion: to.Ptr("DOCKER|reg.azurecr.io/events:abc")},
			want: modeContainer,
		},
		{
			// Only Flex Consumption carries functionAppConfig, and Flex
			// supports no technology other than one deploy.
			name:  "functionAppConfig means Flex Consumption",
			props: &armappservice.SiteProperties{FunctionAppConfig: &armappservice.FunctionAppConfig{}},
			cfg:   &armappservice.SiteConfig{LinuxFxVersion: to.Ptr("Node|20")},
			want:  modeOneDeploy,
		},
		{
			name:  "anything else is a classic plan",
			props: &armappservice.SiteProperties{},
			cfg:   &armappservice.SiteConfig{LinuxFxVersion: to.Ptr("Node|20")},
			want:  modePackageURL,
		},
		{
			// A container on Flex is still a container: the image wins.
			name:  "an image wins over Flex",
			props: &armappservice.SiteProperties{FunctionAppConfig: &armappservice.FunctionAppConfig{}},
			cfg:   &armappservice.SiteConfig{LinuxFxVersion: to.Ptr("DOCKER|reg/events:abc")},
			want:  modeContainer,
		},
		{
			name: "nothing to go on falls back to the classic plan",
			want: modePackageURL,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectMode(tc.props, tc.cfg); got != tc.want {
				t.Errorf("detectMode = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestVersionFromURL(t *testing.T) {
	// A classic plan needs no version marker: the package URL already says
	// which one it is, so the running version is recovered by matching the
	// configured template against it.
	const tmpl = "https://acct.blob.core.windows.net/functions/purchase/purchase-sha-{{.version}}.zip"

	tests := []struct {
		name    string
		current string
		want    string
	}{
		{
			name:    "recovers the version",
			current: "https://acct.blob.core.windows.net/functions/purchase/purchase-sha-27ec167.zip",
			want:    "27ec167",
		},
		{
			// A URL from somewhere else must not be misread as a version, or
			// the tool would think it is up to date when it is not.
			name:    "a URL that does not match yields nothing",
			current: "https://other.blob.core.windows.net/whatever.zip",
			want:    "",
		},
		{
			name:    "an empty setting yields nothing",
			current: "",
			want:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionFromURL(tmpl, tc.current); got != tc.want {
				t.Errorf("versionFromURL = %q, want %q", got, tc.want)
			}
		})
	}

	if got := versionFromURL("https://acct/no-placeholder.zip", "https://acct/no-placeholder.zip"); got != "" {
		t.Errorf("a template without {{.version}} should yield nothing, got %q", got)
	}
}

func TestPayloadEmpty(t *testing.T) {
	// Guards the rollback path: with nothing recorded there is nothing to go
	// back to, and saying so beats pushing an empty payload.
	if !(*functionPayload)(nil).empty() {
		t.Error("a nil payload should be empty")
	}
	if !(&functionPayload{mode: modeOneDeploy}).empty() {
		t.Error("a payload with no url and no image should be empty")
	}
	if (&functionPayload{url: "https://x/y.zip"}).empty() {
		t.Error("a payload with a url is not empty")
	}
	if (&functionPayload{linuxFxVersion: "DOCKER|x:1"}).empty() {
		t.Error("a payload with an image is not empty")
	}
}
