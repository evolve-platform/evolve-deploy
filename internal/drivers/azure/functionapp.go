package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appservice/armappservice/v5"

	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
	"github.com/evolve-platform/evolve-deploy/internal/tmpl"
)

// A function app can be deployed in three quite different ways, and which one
// applies is a property of the app rather than something the config should have
// to repeat. So it is read off the resource:
//
//   - container: the app runs an image, and a deploy is a tag change like every
//     other container target.
//   - one deploy: Flex Consumption, which supports no other technology. The
//     package is fetched from blob storage and posted to the app's publish
//     endpoint.
//   - package URL: the classic plans, where WEBSITE_RUN_FROM_PACKAGE points at
//     a blob and the app fetches it itself.
type funcMode int

const (
	modeContainer funcMode = iota
	modeOneDeploy
	modePackageURL
)

func (m funcMode) String() string {
	switch m {
	case modeContainer:
		return "container"
	case modeOneDeploy:
		return "one-deploy"
	default:
		return "package-url"
	}
}

const (
	// versionSetting records which package a Flex Consumption app runs. One
	// deploy leaves no trace of which zip it pushed, so without this the tool
	// cannot tell whether an app is up to date and would redeploy every run.
	// The other two modes need no marker: the version is visible in the image
	// tag or in the package URL.
	versionSetting = "EVOLVE_DEPLOY_VERSION"
	// packageSetting is how the classic plans are pointed at a package.
	packageSetting = "WEBSITE_RUN_FROM_PACKAGE"

	deployTimeout = 10 * time.Minute
	armScope      = "https://management.azure.com/.default"
	armEndpoint   = "https://management.azure.com"
	sitesAPI      = "2016-08-01"
)

type functionPayload struct {
	mode funcMode

	// container mode
	linuxFxVersion string

	// package modes
	url      string
	settings map[string]*string

	previous *functionPayload
}

func (p *functionPayload) empty() bool {
	return p == nil || (p.url == "" && p.linuxFxVersion == "")
}

// planFunctionApp works out what a function app should be running.
func (d *Driver) planFunctionApp(ctx context.Context, want *target.Desired) (*target.Change, error) {
	t := want.Target

	if want.ManageEnv {
		// App settings on a function app hold platform wiring alongside
		// application config — AzureWebJobsStorage, the deployment connection
		// string, Application Insights. Owning that map would mean reproducing
		// secrets the platform manages, so the tool only ever writes its own
		// keys.
		return nil, fmt.Errorf(
			"%s: env is not supported on function-app targets, because their app settings "+
				"also hold platform wiring (AzureWebJobsStorage, the deployment connection "+
				"string). Run the app on Container Apps if you want its environment managed here",
			t.Label())
	}

	site, err := d.sites.Get(ctx, d.file.Cloud.ResourceGroup, t.Name, nil)
	if err != nil {
		return nil, fmt.Errorf("function app %s: %w", t.Name, err)
	}
	cfg, err := d.sites.GetConfiguration(ctx, d.file.Cloud.ResourceGroup, t.Name, nil)
	if err != nil {
		return nil, fmt.Errorf("function app %s configuration: %w", t.Name, err)
	}

	mode := detectMode(site.Properties, cfg.Properties)
	slog.Debug("function app mode", "app", t.Name, "mode", mode.String())

	if mode == modeContainer {
		return d.planFunctionContainer(want, cfg.Properties)
	}
	return d.planFunctionPackage(ctx, want, mode)
}

// detectMode reads the deployment style off the resource.
func detectMode(props *armappservice.SiteProperties, cfg *armappservice.SiteConfig) funcMode {
	if cfg != nil && strings.HasPrefix(derefString(cfg.LinuxFxVersion), "DOCKER|") {
		return modeContainer
	}
	// Only Flex Consumption carries functionAppConfig, and Flex supports no
	// deployment technology other than one deploy.
	if props != nil && props.FunctionAppConfig != nil {
		return modeOneDeploy
	}
	return modePackageURL
}

// planFunctionContainer treats the app like any other container: the registry
// and repository come from what is deployed, and only the tag changes.
func (d *Driver) planFunctionContainer(
	want *target.Desired, cfg *armappservice.SiteConfig,
) (*target.Change, error) {
	t := want.Target
	if t.Code != nil && t.Code.URL != "" {
		return nil, fmt.Errorf(
			"%s runs a container image, so `code.url` does not apply to it", t.Label())
	}

	current := derefString(cfg.LinuxFxVersion)
	ref := strings.TrimPrefix(current, "DOCKER|")

	next, err := image.Retag(ref, want.Version)
	if err != nil {
		return nil, err
	}
	from := image.Tag(ref)
	if from == want.Version {
		return nil, nil
	}

	return &target.Change{
		Service:     want.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   want.Version,
		Reason:      reason(from, want.Version),
		Payload: &functionPayload{
			mode:           modeContainer,
			linuxFxVersion: "DOCKER|" + next,
			previous:       &functionPayload{mode: modeContainer, linuxFxVersion: current},
		},
	}, nil
}

// planFunctionPackage covers both zip styles. They differ only in how the
// package reaches the app, not in where it comes from.
func (d *Driver) planFunctionPackage(
	ctx context.Context, want *target.Desired, mode funcMode,
) (*target.Change, error) {
	t := want.Target
	if t.Code == nil || t.Code.URL == "" {
		return nil, fmt.Errorf(
			"%s deploys a package (%s), so it needs `code.url`", t.Label(), mode)
	}

	url, err := tmpl.Render(t.Code.URL, map[string]string{
		"version": want.Version, "name": t.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("code.url: %w", err)
	}

	settings, err := d.sites.ListApplicationSettings(ctx, d.file.Cloud.ResourceGroup, t.Name, nil)
	if err != nil {
		return nil, fmt.Errorf("function app %s: %w", t.Name, err)
	}
	current := map[string]*string{}
	for k, v := range settings.Properties {
		current[k] = v
	}

	// Where the running version is read from differs by mode: one deploy leaves
	// no trace, while a package URL contains it.
	from := ""
	if mode == modeOneDeploy {
		if v := current[versionSetting]; v != nil {
			from = *v
		}
	} else if v := current[packageSetting]; v != nil {
		from = versionFromURL(t.Code.URL, *v)
	}
	if from == want.Version {
		return nil, nil
	}

	// Only checked for one deploy, which reads the package itself. Under
	// WEBSITE_RUN_FROM_PACKAGE the app fetches it, so the tool needs no access
	// to the blob — and checking it would demand a permission the deploy does
	// not otherwise need.
	if mode == modeOneDeploy {
		if err := d.verifyPackage(ctx, url); err != nil {
			return nil, err
		}
	}

	next := map[string]*string{}
	for k, v := range current {
		next[k] = v
	}
	if mode == modeOneDeploy {
		next[versionSetting] = to.Ptr(want.Version)
	} else {
		next[packageSetting] = to.Ptr(url)
	}

	previous := &functionPayload{mode: mode, settings: current}
	if from != "" {
		previous.url, _ = tmpl.Render(t.Code.URL, map[string]string{
			"version": from, "name": t.Name,
		})
	}

	return &target.Change{
		Service:     want.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   want.Version,
		Reason:      reason(from, want.Version),
		Payload: &functionPayload{
			mode: mode, url: url, settings: next, previous: previous,
		},
	}, nil
}

func (d *Driver) applyFunctionApp(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*functionPayload)

	if err := d.pushFunction(ctx, ch.Target.Name, p); err != nil {
		if p.previous.empty() {
			return fmt.Errorf("%w (no previous version recorded, so nothing to roll back to)", err)
		}
		if rbErr := d.pushFunction(ctx, ch.Target.Name, p.previous); rbErr != nil {
			return errors.Join(
				fmt.Errorf("rollback to %s also failed", orNone(ch.FromVersion)), err, rbErr)
		}
		return fmt.Errorf("rolled back to %s: %w", orNone(ch.FromVersion), err)
	}
	return nil
}

func (d *Driver) revertFunctionApp(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*functionPayload)
	if p.previous.empty() {
		return fmt.Errorf("no previous version recorded for %s", ch.Target.Name)
	}
	return d.pushFunction(ctx, ch.Target.Name, p.previous)
}

func (d *Driver) pushFunction(ctx context.Context, name string, p *functionPayload) error {
	switch p.mode {
	case modeContainer:
		_, err := d.sites.CreateOrUpdateConfiguration(ctx, d.file.Cloud.ResourceGroup, name,
			armappservice.SiteConfigResource{
				Properties: &armappservice.SiteConfig{
					LinuxFxVersion: to.Ptr(p.linuxFxVersion),
				},
			}, nil)
		if err != nil {
			return fmt.Errorf("setting the container image: %w", err)
		}
		return nil

	case modeOneDeploy:
		target.Status(ctx, "the package to be downloaded")
		timer := logging.Start("fetch package", "app", name, "url", p.url)
		zip, err := d.download(ctx, p.url)
		if err != nil {
			return err
		}
		timer.Done("bytes", len(zip))

		if err := d.oneDeploy(ctx, name, zip); err != nil {
			return err
		}
		if _, err := d.sites.UpdateApplicationSettings(ctx, d.file.Cloud.ResourceGroup, name,
			armappservice.StringDictionary{Properties: p.settings}, nil); err != nil {
			return fmt.Errorf("recording the deployed version: %w", err)
		}
		return nil

	default:
		if _, err := d.sites.UpdateApplicationSettings(ctx, d.file.Cloud.ResourceGroup, name,
			armappservice.StringDictionary{Properties: p.settings}, nil); err != nil {
			return fmt.Errorf("setting %s: %w", packageSetting, err)
		}
		// Pointing an app at a new package does not tell the Functions
		// infrastructure that its triggers changed. Without this the app runs
		// the new code but keeps the old trigger registrations.
		return d.syncTriggers(ctx, name)
	}
}

// syncTriggers is a raw ARM call: the SDK exposes no method for it.
func (d *Driver) syncTriggers(ctx context.Context, name string) error {
	url := fmt.Sprintf(
		"%s/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Web/sites/%s/syncfunctiontriggers?api-version=%s",
		armEndpoint, d.file.Cloud.Subscription, d.file.Cloud.ResourceGroup, name, sitesAPI)

	if _, err := d.post(ctx, url, nil, "application/json"); err != nil {
		return fmt.Errorf("syncing triggers for %s: %w", name, err)
	}
	slog.Debug("triggers synced", "app", name)
	return nil
}

// oneDeploy posts the package to the app's SCM site and waits for it to settle.
func (d *Driver) oneDeploy(ctx context.Context, name string, zip []byte) error {
	host, err := d.scmHost(ctx, name)
	if err != nil {
		return err
	}

	target.Status(ctx, "the package to be accepted")
	timer := logging.Start("one deploy", "app", name, "host", host, "bytes", len(zip))
	if _, err := d.post(ctx,
		"https://"+host+"/api/publish?RemoteBuild=false", zip, "application/zip"); err != nil {
		return fmt.Errorf("one deploy to %s: %w", host, err)
	}
	timer.Done()

	return d.waitDeployment(ctx, host)
}

// post sends an authenticated request using the same credential chain as
// everything else, and turns a non-2xx into an error carrying the body — which
// is where App Service puts the reason.
func (d *Driver) post(ctx context.Context, url string, body []byte, contentType string) ([]byte, error) {
	token, err := d.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{armScope}})
	if err != nil {
		return nil, fmt.Errorf("acquiring a token: %w", err)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Content-Type", contentType)

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	out, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// waitDeployment polls until the deployment reports complete. Acceptance of the
// package only means it was received; the app still has to unpack it and
// restart, and that is where a broken package shows up.
func (d *Driver) waitDeployment(ctx context.Context, host string) error {
	token, err := d.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{armScope}})
	if err != nil {
		return err
	}

	target.Status(ctx, "the deployment to unpack and restart")

	started := time.Now()
	deadline := started.Add(deployTimeout)

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://"+host+"/api/deployments/latest", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token.Token)

		resp, err := d.http.Do(req)
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", host, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()

		var status struct {
			Status     int    `json:"status"`
			StatusText string `json:"status_text"`
			Complete   bool   `json:"complete"`
			LogURL     string `json:"log_url"`
		}
		_ = json.Unmarshal(body, &status)

		slog.Debug("deployment poll", "host", host, "status", status.Status,
			"complete", status.Complete, "elapsed", time.Since(started).Round(time.Second))

		// Kudu reports 3 for failed and 4 for success.
		switch {
		case status.Status == 3:
			return fmt.Errorf("deployment to %s failed: %s (%s)",
				host, status.StatusText, status.LogURL)
		case status.Complete && status.Status == 4:
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("deployment to %s did not complete within %s (last status %d)",
				host, deployTimeout, status.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// scmHost finds the app's deployment endpoint, read off the resource rather
// than assembled from the name: an app on a custom or sovereign domain does not
// follow the azurewebsites.net pattern.
func (d *Driver) scmHost(ctx context.Context, name string) (string, error) {
	site, err := d.sites.Get(ctx, d.file.Cloud.ResourceGroup, name, nil)
	if err != nil {
		return "", fmt.Errorf("function app %s: %w", name, err)
	}
	for _, h := range site.Properties.EnabledHostNames {
		if h != nil && strings.Contains(*h, ".scm.") {
			return *h, nil
		}
	}
	return "", fmt.Errorf("function app %s exposes no scm host name", name)
}

// versionFromURL recovers the running version by matching the configured
// template against the URL the app is pointed at, so no marker is needed.
func versionFromURL(template, current string) string {
	const placeholder = "{{.version}}"
	i := strings.Index(template, placeholder)
	if i < 0 {
		return ""
	}
	pattern := "^" + regexp.QuoteMeta(template[:i]) + "(.+)" +
		regexp.QuoteMeta(template[i+len(placeholder):]) + "$"

	re, err := regexp.Compile(pattern)
	if err != nil {
		return ""
	}
	if m := re.FindStringSubmatch(current); m != nil {
		return m[1]
	}
	return ""
}
