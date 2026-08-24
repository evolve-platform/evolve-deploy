// Package azure implements the Container App and Container App Job targets.
//
// The shape of a reference differs here from AWS, and the difference is not
// cosmetic. On ECS a task definition carries a Secrets Manager ARN directly. A
// Container App cannot: a secret has to be *declared* on the app — with a Key
// Vault URL and the managed identity that may read it — and the environment
// then refers to it by name. Declaring it is Terraform's job and already
// happens; this tool only points at what is there. So `${secret:ctp-secret}`
// means "the secret named ctp-secret that Terraform declared on this app", and
// evolve-deploy never touches Key Vault at all.
package azure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azappconfig"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appservice/armappservice/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// appContainer is what the app-container and app-container-job modules call the
// application container; the sidecar beside it is "reverse-proxy". Only
// consulted when a template has several containers and none is named in the
// config.
const appContainer = "main"

// Driver is the Azure implementation of target.Driver.
type Driver struct {
	file *config.File

	apps  *armappcontainers.ContainerAppsClient
	jobs  *armappcontainers.JobsClient
	sites *armappservice.WebAppsClient

	// revisions and replicas are only read while waiting for a rollout. The app
	// resource says whether a revision is ready yet; only these say whether it
	// is ever going to be.
	revisions *armappcontainers.ContainerAppsRevisionsClient
	replicas  *armappcontainers.ContainerAppsRevisionReplicasClient

	// cred is kept because the function-app driver talks to the SCM site over
	// plain HTTP rather than through ARM, and has to fetch its own token.
	cred azcore.TokenCredential
	http *http.Client

	// appConfig is built only when needed: most repositories never use ${param:…}, and
	// requiring an App Configuration endpoint from every one of them would be
	// configuration for its own sake.
	appConfig *azappconfig.Client

	// poll is how often a wait re-reads the app and the revision under it. A
	// field rather than the constant it is set from, so that a test of the wait
	// itself does not have to spend five seconds per poll to find out what it
	// decided.
	poll time.Duration
}

// New builds a driver from the ambient credential chain — in CI that is the
// federated token from the login-azure action, locally an az login session.
func New(_ context.Context, f *config.File) (*Driver, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure: no usable credentials: %w", err)
	}
	return newDriver(f, cred, nil)
}

// newDriver builds the clients against whatever the options point at.
//
// Split from New because the credential chain is the one part of this that
// cannot be faked, and everything interesting is on the other side of it: the
// code that decides whether a rollout failed is only reachable through a
// client, so a test hands this a credential that signs nothing and an ARM
// transport that answers from a table. In production both arguments come from
// New and nothing here behaves differently.
func newDriver(f *config.File, cred azcore.TokenCredential, opts *arm.ClientOptions) (*Driver, error) {
	apps, err := armappcontainers.NewContainerAppsClient(f.Cloud.Subscription, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	jobs, err := armappcontainers.NewJobsClient(f.Cloud.Subscription, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	sites, err := armappservice.NewWebAppsClient(f.Cloud.Subscription, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	revisions, err := armappcontainers.NewContainerAppsRevisionsClient(f.Cloud.Subscription, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	replicas, err := armappcontainers.NewContainerAppsRevisionReplicasClient(f.Cloud.Subscription, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}

	d := &Driver{
		file: f, apps: apps, jobs: jobs, sites: sites, cred: cred,
		revisions: revisions, replicas: replicas,
		// Long enough for a package upload on a slow link; the deployment
		// itself is waited for separately.
		http: &http.Client{Timeout: 5 * time.Minute},
		poll: pollInterval,
	}

	if f.Cloud.AppConfig != "" {
		d.appConfig, err = azappconfig.NewClient(f.Cloud.AppConfig, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: app_config %s: %w", f.Cloud.AppConfig, err)
		}
	}
	return d, nil
}

func (d *Driver) Name() string { return "azure" }

// Capabilities: a Container App resolves a secret reference itself, but only
// one that is already declared on it. It has no equivalent for App
// Configuration, so ${param:…} is read here and written out as a literal.
func (d *Driver) Capabilities(t config.TargetType) target.Capability {
	switch t {
	case config.TypeContainerApp, config.TypeContainerJob:
		return target.Capability{NativeSecret: true}
	default:
		// Function apps manage no environment here at all, so no reference of
		// either kind ever reaches them.
		return target.Capability{}
	}
}

// Verify has nothing to guard.
//
// Unlike AWS, where the account is implicit in the credentials and a wrong role
// silently deploys somewhere else, the subscription and resource group are
// parameters on every call here. Naming the wrong one produces a "not found",
// not a deploy to the wrong place.
func (d *Driver) Verify(_ context.Context) error { return nil }

func (d *Driver) Resolver() refs.Resolver { return &resolver{d: d} }

func (d *Driver) Plan(ctx context.Context, want *target.Desired) (*target.Change, error) {
	switch want.Target.Type {
	case config.TypeContainerApp:
		return d.planApp(ctx, want)
	case config.TypeContainerJob:
		return d.planJob(ctx, want)
	case config.TypeFunctionApp:
		return d.planFunctionApp(ctx, want)
	default:
		return nil, &target.ErrNotImplemented{Cloud: "azure", Type: want.Target.Type}
	}
}

func (d *Driver) Apply(ctx context.Context, ch *target.Change) error {
	switch ch.Target.Type {
	case config.TypeContainerApp:
		return d.applyApp(ctx, ch)
	case config.TypeContainerJob:
		return d.applyJob(ctx, ch)
	case config.TypeFunctionApp:
		return d.applyFunctionApp(ctx, ch)
	default:
		return &target.ErrNotImplemented{Cloud: "azure", Type: ch.Target.Type}
	}
}

func (d *Driver) Revert(ctx context.Context, ch *target.Change) error {
	switch ch.Target.Type {
	case config.TypeContainerApp:
		return d.revertApp(ctx, ch)
	case config.TypeContainerJob:
		return d.revertJob(ctx, ch)
	case config.TypeFunctionApp:
		return d.revertFunctionApp(ctx, ch)
	default:
		return &target.ErrNotImplemented{Cloud: "azure", Type: ch.Target.Type}
	}
}

// renderEnv turns the planned environment into Container App environment
// variables. A reference has already been checked to exist as a declared secret
// on this resource, so it becomes a secretRef; everything else is a literal.
func renderEnv(env []target.EnvVar) []*armappcontainers.EnvironmentVar {
	out := make([]*armappcontainers.EnvironmentVar, 0, len(env))
	for _, e := range env {
		if e.Value.IsRef() {
			out = append(out, &armappcontainers.EnvironmentVar{
				Name:      to.Ptr(e.Name),
				SecretRef: to.Ptr(e.Value.Name),
			})
			continue
		}
		out = append(out, &armappcontainers.EnvironmentVar{
			Name:  to.Ptr(e.Name),
			Value: to.Ptr(e.Value.Literal),
		})
	}
	return out
}

// envFingerprint flattens an environment for comparison. A reference is
// compared by what it points at, not by its value: the tool cannot see the
// value and should not pretend a rotated secret is a change.
func envFingerprint(env []*armappcontainers.EnvironmentVar, ignore []string) map[string]string {
	skip := map[string]bool{target.SideEnvVar: true}
	for _, name := range ignore {
		skip[name] = true
	}

	out := make(map[string]string, len(env))
	for _, e := range env {
		// The side is the tool's own and alternates every release, so the
		// desired value never matches what the serving revision carries.
		// Comparing it would report an environment change on every run, which
		// would deploy on every run, which would flip the sides forever with no
		// version ever changing. Anything the config sets per side is skipped
		// for exactly that reason too.
		if e != nil && skip[derefString(e.Name)] {
			continue
		}
		switch {
		case e == nil || e.Name == nil:
		case e.SecretRef != nil:
			out[*e.Name] = "->" + *e.SecretRef
		default:
			out[*e.Name] = "=" + derefString(e.Value)
		}
	}
	return out
}

func containerNames(containers []*armappcontainers.Container) []string {
	out := make([]string, 0, len(containers))
	for _, c := range containers {
		if c != nil {
			out = append(out, derefString(c.Name))
		}
	}
	return out
}

func findContainer(containers []*armappcontainers.Container, name string) *armappcontainers.Container {
	for _, c := range containers {
		if c != nil && derefString(c.Name) == name {
			return c
		}
	}
	return nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefBool(b *bool) bool {
	return b != nil && *b
}

func derefInt32(i *int32) int32 {
	if i == nil {
		return 0
	}
	return *i
}

// --- references --------------------------------------------------------------

type resolver struct{ d *Driver }

// Verify checks a reference at plan time.
//
// A secret cannot be checked here: whether it exists is a property of the
// resource it is declared on, not of a store, so it is checked while planning
// that target. A parameter is read from App Configuration, which has no
// metadata-only lookup — but parameters hold configuration, and anything
// sensitive belongs in a secret.
func (r *resolver) Verify(ctx context.Context, v refs.Value) error {
	if v.Kind != refs.Param {
		return nil
	}
	_, err := r.Read(ctx, v)
	return err
}

func (r *resolver) Read(ctx context.Context, v refs.Value) (string, error) {
	switch v.Kind {
	case refs.Param:
		if r.d.appConfig == nil {
			return "", fmt.Errorf(
				"%s needs an App Configuration endpoint — set `app_config` in %s",
				v.Raw, r.d.file.Path)
		}
		out, err := r.d.appConfig.GetSetting(ctx, v.Name, nil)
		if err != nil {
			var respErr *azcore.ResponseError
			if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
				return "", fmt.Errorf("parameter %q does not exist in App Configuration", v.Name)
			}
			return "", fmt.Errorf("parameter %q: %w", v.Name, err)
		}
		return derefString(out.Value), nil

	case refs.Secret:
		// Reaching here means a target reported that it cannot carry a secret
		// reference. None of the Azure targets do, so this is a bug rather than
		// something to paper over by reading Key Vault.
		return "", fmt.Errorf(
			"%s cannot be resolved: on Azure a secret is declared on the resource and referenced by name", v.Raw)

	default:
		return v.Literal, nil
	}
}

func (r *resolver) ReadMap(ctx context.Context, v refs.Value) (map[string]string, error) {
	raw, err := r.Read(ctx, v)
	if err != nil {
		return nil, err
	}
	return parseJSONMap(v.Raw, raw)
}

// verifySecretRefs checks that every secret a target refers to is declared on
// that resource. Without this the deploy succeeds and the revision fails to
// start, which is a much more expensive way to find a typo.
func verifySecretRefs(env []target.EnvVar, declared []*armappcontainers.Secret, where string) error {
	if len(env) == 0 {
		return nil
	}

	names := make(map[string]bool, len(declared))
	for _, s := range declared {
		if s != nil && s.Name != nil {
			names[*s.Name] = true
		}
	}

	var missing []string
	for _, e := range env {
		if e.Value.IsRef() && !names[e.Value.Name] {
			missing = append(missing, fmt.Sprintf("%s (%s)", e.Value.Name, e.Name))
		}
	}
	if len(missing) == 0 {
		return nil
	}

	have := "none"
	if len(names) > 0 {
		have = strings.Join(sortedNames(names), ", ")
	}
	return fmt.Errorf(
		"%s does not declare secret %s — Terraform declares secrets on the resource, this tool only refers to them (declared: %s)",
		where, strings.Join(missing, ", "), have)
}

// --- deployment packages ------------------------------------------------------

// blobFor builds a client straight from the package URL.
//
// Not by splitting the URL into account, container and path and reassembling
// it: a package lives at functions/<service>/<service>-sha-<version>.zip, and
// reassembling that escapes the slash in the middle into %2F, which points at a
// blob that does not exist.
func (d *Driver) blobFor(raw string) (*blob.Client, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path == "" {
		return nil, fmt.Errorf(
			"%q is not a blob URL of the form https://<account>.blob.core.windows.net/<container>/<path>", raw)
	}
	return blob.NewClient(raw, d.cred, nil)
}

// verifyPackage checks the zip exists before anything is deployed. The pipeline
// hands over a version whether or not its build succeeded, so without this the
// deploy starts and then fails on a package that was never uploaded.
func (d *Driver) verifyPackage(ctx context.Context, raw string) error {
	client, err := d.blobFor(raw)
	if err != nil {
		return err
	}
	if _, err := client.GetProperties(ctx, nil); err != nil {
		return fmt.Errorf("package %s: %w", raw, err)
	}
	return nil
}

func (d *Driver) download(ctx context.Context, raw string) ([]byte, error) {
	client, err := d.blobFor(raw)
	if err != nil {
		return nil, err
	}
	resp, err := client.DownloadStream(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", raw, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", raw, err)
	}
	return data, nil
}
