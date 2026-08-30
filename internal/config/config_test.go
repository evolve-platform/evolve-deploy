package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tst.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, body string) (*File, error) {
	t.Helper()
	return Load(write(t, body), "tst")
}

const awsHeader = `
cloud:
  provider: aws
  account: "513712104672"
  region: eu-west-1
`

func TestShortFormExpandsToOneTarget(t *testing.T) {
	f, err := load(t, awsHeader+`
services:
  site:
    version: abc1234
    type: ecs
    cluster: platform
`)
	if err != nil {
		t.Fatal(err)
	}

	site := f.Services["site"]
	if site.Name != "site" {
		t.Errorf("Name = %q, want site", site.Name)
	}
	if len(site.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(site.Targets))
	}

	target := site.Targets[0]
	if target.Name != "site" {
		t.Errorf("target name = %q, want site", target.Name)
	}
	// The base family Terraform owns is derived from the name, so the common
	// case needs no extra lines.
	if target.Base != "site-base" {
		t.Errorf("Base = %q, want site-base", target.Base)
	}
	// Container stays empty: which container carries the image depends on what
	// is actually in the task, so the driver resolves it. Defaulting to the
	// target name here would be wrong — the ecs-service module calls it "app"
	// and Container Apps calls it "main", both with a sidecar beside them.
	if target.Container != "" {
		t.Errorf("Container = %q, want it left to the driver", target.Container)
	}
}

func TestTargetEnvOverridesComponentEnv(t *testing.T) {
	f, err := load(t, awsHeader+`
services:
  discover:
    version: abc1234
    cluster: platform
    env:
      LOG_LEVEL: info
      SHARED: yes
    targets:
      - type: ecs
        name: discover
        env:
          LOG_LEVEL: debug
      - type: ecs
        name: discover-sync
`)
	if err != nil {
		t.Fatal(err)
	}

	targets := f.Services["discover"].Targets
	if got := targets[0].Env["LOG_LEVEL"]; got != "debug" {
		t.Errorf("target override LOG_LEVEL = %q, want debug", got)
	}
	if got := targets[0].Env["SHARED"]; got != "yes" {
		t.Errorf("inherited SHARED = %q, want yes", got)
	}
	if got := targets[1].Env["LOG_LEVEL"]; got != "info" {
		t.Errorf("sibling LOG_LEVEL = %q, want info (service value)", got)
	}
}

func TestComponentDefaultsAreInheritedPerType(t *testing.T) {
	// A service with both an ECS service and a Lambda can set cluster once
	// without the Lambda picking up a field that means nothing to it — which
	// would then be rejected by validation.
	f, err := load(t, awsHeader+`
services:
  purchase:
    version: ghi9012
    cluster: platform
    code: { bucket: artifacts, key: "purchase-{{.version}}.zip" }
    targets:
      - { type: ecs, name: purchase }
      - { type: lambda, name: purchase-events }
`)
	if err != nil {
		t.Fatal(err)
	}

	targets := f.Services["purchase"].Targets
	if targets[0].Cluster != "platform" {
		t.Errorf("ecs cluster = %q, want platform", targets[0].Cluster)
	}
	if targets[0].Code != nil {
		t.Error("the ecs target inherited a lambda-only field")
	}
	if targets[1].Cluster != "" {
		t.Errorf("the lambda inherited cluster = %q", targets[1].Cluster)
	}
	if targets[1].Code == nil || targets[1].Code.Bucket != "artifacts" {
		t.Errorf("lambda code = %+v", targets[1].Code)
	}
}

func TestVersionIsSharedByEveryTarget(t *testing.T) {
	// The reason version lives on the service: a service and its jobs run one
	// image, so the version cannot be written twice and drift.
	f, err := load(t, awsHeader+`
services:
  purchase:
    version: ghi9012
    targets:
      - { type: ecs, name: purchase, cluster: platform }
      - type: lambda
        name: purchase-events
        code: { bucket: artifacts, key: "purchase-sha-{{.version}}.zip" }
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Services["purchase"].Version; got != "ghi9012" {
		t.Errorf("Version = %q", got)
	}
	if n := len(f.Services["purchase"].Targets); n != 2 {
		t.Errorf("got %d targets, want 2", n)
	}
}

func TestHooksTakeEitherForm(t *testing.T) {
	// The plain string is what every config in existence is written in, and it
	// has to go on meaning the same thing next to the named actions.
	f, err := load(t, awsHeader+`
services:
  site:
    version: abc1234
    type: ecs
    cluster: c
    before:
      - hive schema:check --commit {{.version}}
      - cmd: echo explicit
    after:
      - {uses: honeycomb, with: {dataset: site}}
`)
	if err != nil {
		t.Fatal(err)
	}

	c := f.Services["site"]
	if len(c.Before) != 2 || len(c.After) != 1 {
		t.Fatalf("got %d before and %d after", len(c.Before), len(c.After))
	}
	if got, want := c.Before[0].Describe(), "hive schema:check --commit {{.version}}"; got != want {
		t.Errorf("first hook = %q, want %q", got, want)
	}
	if got := c.After[0].Describe(); !strings.Contains(got, "honeycomb marker on site") {
		t.Errorf("the action does not describe itself: %q", got)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown key is a typo, not something to ignore",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n    cluster: c\n    clsuter: c\n",
			want: "field clsuter not found",
		},
		{
			name: "a hook mistake names the entry it is in",
			body: awsHeader + `services:
  site:
    version: a
    type: ecs
    cluster: c
    after:
      - echo fine
      - {uses: honycomb, with: {dataset: site}}
`,
			want: `services.site.after[1]: line 13: uses: "honycomb" is not one of honeycomb, http, sentry`,
		},
		{
			name: "an option the action does not have",
			body: awsHeader + `services:
  site:
    version: a
    type: ecs
    cluster: c
    after:
      - {uses: honeycomb, with: {dataset: site, mesage: hi}}
`,
			want: "services.site.after[0]: line 12: with: no such option: mesage",
		},
		{
			name: "a smoke entry is checked the same way",
			body: awsHeader + `strategy:
  type: blue-green
  smoke:
    - {uses: http, with: {retry: 3}}
services:
  site:
    version: a
    targets:
      - { type: ecs, name: site, cluster: c, test_url: https://site.test }
`,
			want: "strategy.smoke[0]: line 9: http: `url` is required",
		},
		{
			name: "type must belong to the cloud",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: cloud-run\n",
			want: "is not valid on aws",
		},
		{
			name: "addressing from another cloud is a half-finished copy",
			body: "cloud:\n  provider: aws\n  account: \"1\"\n  region: eu-west-1\n  project: something\nservices:\n  site:\n    version: a\n    type: ecs\n    cluster: c\n",
			want: "cloud.project: does not apply when provider is aws",
		},
		{
			name: "missing addressing",
			body: "cloud:\n  provider: aws\nservices:\n  site:\n    version: a\n    type: ecs\n    cluster: c\n",
			want: "cloud.account: required when provider is aws",
		},
		{
			name: "version is required",
			body: awsHeader + "services:\n  site:\n    type: ecs\n    cluster: c\n",
			want: "version is required",
		},
		{
			name: "ecs needs a cluster",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n",
			want: "cluster is required",
		},
		{
			name: "lambda needs a code location",
			body: awsHeader + "services:\n  ev:\n    version: a\n    type: lambda\n",
			want: "need `code`",
		},
		{
			name: "cluster on a lambda is a copy-paste mistake",
			body: awsHeader + "services:\n  ev:\n    version: a\n    targets:\n      - type: lambda\n        name: ev\n        cluster: platform\n        code: { bucket: b, key: k }\n",
			want: "`cluster` only applies to ecs",
		},
		{
			name: "neither type nor targets",
			body: awsHeader + "services:\n  site:\n    version: a\n",
			want: "needs either `type`",
		},
		{
			name: "duplicate target",
			body: awsHeader + "services:\n  site:\n    version: a\n    targets:\n      - { type: ecs, name: x, cluster: c }\n      - { type: ecs, name: x, cluster: c }\n",
			want: "duplicate target",
		},
		{
			name: "unknown resolve policy",
			body: awsHeader + "refs:\n  resolve: maybe\nservices:\n  site:\n    version: a\n    type: ecs\n    cluster: c\n",
			want: "refs.resolve",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.body)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error was:\n%v\nwant it to mention %q", err, tc.want)
			}
		})
	}
}

func TestResolveDefaultsToAllow(t *testing.T) {
	f, err := load(t, awsHeader+"services:\n  site:\n    version: a\n    type: ecs\n    cluster: c\n")
	if err != nil {
		t.Fatal(err)
	}
	if f.Refs.Resolve != ResolveAllow {
		t.Errorf("Resolve = %q, want allow", f.Refs.Resolve)
	}
}

func TestSelectComponentsRejectsUnknownNames(t *testing.T) {
	// An unknown --only would otherwise deploy nothing and exit zero, which
	// looks exactly like a successful no-op.
	f, err := load(t, awsHeader+"services:\n  site:\n    version: a\n    type: ecs\n    cluster: c\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SelectServices([]string{"nope"}); err == nil {
		t.Fatal("SelectServices accepted an unknown name")
	}
	if err := f.SelectServices([]string{"site"}); err != nil {
		t.Fatalf("SelectServices(site): %v", err)
	}
	if len(f.Services) != 1 {
		t.Errorf("got %d services after selecting one", len(f.Services))
	}
}

func TestSetVersionsRejectsUnknownNames(t *testing.T) {
	f, err := load(t, awsHeader+"services:\n  site:\n    version: a\n    type: ecs\n    cluster: c\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SetVersions(map[string]string{"nope": "x"}); err == nil {
		t.Fatal("SetVersions accepted an unknown service")
	}
	if err := f.SetVersions(map[string]string{"site": "def5678"}); err != nil {
		t.Fatal(err)
	}
	if got := f.Services["site"].Version; got != "def5678" {
		t.Errorf("Version = %q, want def5678", got)
	}
}

func TestManagesEnvIsOnlyTrueWhenTheConfigSaysSomething(t *testing.T) {
	// The difference between "leave the environment alone" and "the environment
	// is now empty". Without it, the image-only mode would wipe every variable
	// Terraform set.
	f, err := load(t, awsHeader+`
services:
  quiet:
    version: a
    type: ecs
    cluster: c
  with-env:
    version: a
    type: ecs
    cluster: c
    env:
      LOG_LEVEL: info
  with-bulk:
    version: a
    type: ecs
    cluster: c
    envFrom:
      - ${param:/x}
  target-only:
    version: a
    cluster: c
    targets:
      - type: ecs
        name: target-only
        env:
          LOG_LEVEL: info
`)
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]bool{
		"quiet":       false,
		"with-env":    true,
		"with-bulk":   true,
		"target-only": true,
	} {
		if got := f.Services[name].Targets[0].ManagesEnv; got != want {
			t.Errorf("%s: ManagesEnv = %v, want %v", name, got, want)
		}
	}
}

func TestDependsOnMustNameAServiceInTheFile(t *testing.T) {
	// A typo would otherwise be silent: the dependency is simply never found
	// in the run, and the service deploys straight away.
	_, err := load(t, awsHeader+`
services:
  site:
    version: v2
    type: ecs
    cluster: platform
    depends_on: [discovr]
`)
	if err == nil {
		t.Fatal("a dependency on a service that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "discovr") {
		t.Errorf("the error does not name the typo: %v", err)
	}
}

func TestDependsOnRefusesACycle(t *testing.T) {
	_, err := load(t, awsHeader+`
services:
  a:
    version: v2
    type: ecs
    cluster: platform
    depends_on: [c]
  b:
    version: v2
    type: ecs
    cluster: platform
    depends_on: [a]
  c:
    version: v2
    type: ecs
    cluster: platform
    depends_on: [b]
`)
	if err == nil {
		t.Fatal("a cycle was accepted; the apply would have deadlocked")
	}
	// The path is the point — "there is a cycle" leaves you to find it.
	for _, name := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s is missing from the cycle path: %v", name, err)
		}
	}
}

func TestDependsOnRefusesItself(t *testing.T) {
	_, err := load(t, awsHeader+`
services:
  site:
    version: v2
    type: ecs
    cluster: platform
    depends_on: [site]
`)
	if err == nil {
		t.Fatal("a service depending on itself was accepted")
	}
}

// A job is a target type of one cloud, and the table that says so is the whole
// check — so both halves of it are worth a line here.
func TestACloudRunJobBelongsToGCPOnly(t *testing.T) {
	const gcpHeader = `
cloud:
  provider: gcp
  project: evolve-tst
  region: europe-west4
`
	f, err := load(t, gcpHeader+`
services:
  discover:
    version: abc1234
    targets:
      - { type: cloud-run,     name: evolve-tst-discover }
      - { type: cloud-run-job, name: evolve-tst-discover-products }
`)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.Services["discover"].Targets); n != 2 {
		t.Fatalf("got %d targets, want 2", n)
	}

	_, err = load(t, awsHeader+"services:\n  site:\n    version: a\n    type: cloud-run-job\n")
	if err == nil || !strings.Contains(err.Error(), "is not valid on aws") {
		t.Errorf("err = %v, want a refusal naming aws", err)
	}
}

// Cloud Run containers have no conventional name, so PickContainer cannot guess
// which one carries the image once a service runs a sidecar. Saying so in the
// config is the only way through, and it used to be refused as ECS-only --
// which left a Cloud Run service with a reverse proxy undeployable either way.
func TestContainerAppliesBeyondECS(t *testing.T) {
	const gcpHeader = `
cloud:
  provider: gcp
  project: evolve-tst
  region: europe-west4
`
	f, err := load(t, gcpHeader+`
services:
  graphql-gateway:
    version: abc1234
    targets:
      - { type: cloud-run, name: graphql-gateway, container: graphql-gateway-1 }
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Services["graphql-gateway"].Targets[0].Container; got != "graphql-gateway-1" {
		t.Errorf("Container = %q, want graphql-gateway-1", got)
	}

	// Still refused where no driver reads it, which is what catches a target
	// quietly ignoring half its configuration.
	_, err = load(t, awsHeader+"services:\n  ev:\n    version: a\n    targets:\n      - type: lambda\n        name: ev\n        container: main\n        code: { bucket: b, key: k }\n")
	if err == nil || !strings.Contains(err.Error(), "`container` does not apply to lambda") {
		t.Errorf("err = %v, want a refusal naming lambda", err)
	}
}

// A command is what makes one job different from the next when several share an
// image, and it names a path inside that image -- so it has to be able to travel
// with the version rather than sitting in Terraform, where a layout change and
// the command that follows it land in two separate writes.
func TestCommandIsATargetFieldOnTheTypesThatWriteIt(t *testing.T) {
	const gcpHeader = `
cloud:
  provider: gcp
  project: evolve-tst
  region: europe-west4
`
	f, err := load(t, gcpHeader+`
services:
  discover:
    version: abc1234
    targets:
      - type: cloud-run
        name: discover
      - type: cloud-run-job
        name: discover-products
        command: ["node", "src/cli.ts", "sync", "products"]
`)
	if err != nil {
		t.Fatal(err)
	}
	targets := f.Services["discover"].Targets
	if got := targets[0].Command; got != nil {
		t.Errorf("service command = %v, want none declared", got)
	}
	if got := strings.Join(targets[1].Command, " "); got != "node src/cli.ts sync products" {
		t.Errorf("job command = %q", got)
	}

	// Every type that runs a container takes one, on all three clouds.
	f, err = load(t, `
cloud:
  provider: azure
  subscription: 00000000-0000-0000-0000-000000000000
  resource_group: evolve-tst
services:
  site:
    version: abc1234
    targets:
      - { type: container-app,     name: site,      command: ["node", "server.js"] }
      - { type: container-app-job, name: site-sync, command: ["node", "sync.js"] }
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Services["site"].Targets[1].Command, " "); got != "node sync.js" {
		t.Errorf("container-app-job command = %q", got)
	}

	// Refused where the host owns the entry point: a lambda and a function app
	// are handed a package, not started with a command line.
	_, err = load(t, awsHeader+"services:\n  ev:\n    version: a\n    targets:\n      - type: lambda\n        name: ev\n        command: [\"node\", \"index.js\"]\n        code: { bucket: b, key: k }\n")
	if err == nil || !strings.Contains(err.Error(), "`command` does not apply to lambda") {
		t.Errorf("err = %v, want a refusal naming lambda", err)
	}

	// An empty list has no meaning worth guessing at: clearing an entry point
	// leaves the image's CMD, and omitting the key already says "leave it".
	_, err = load(t, gcpHeader+`
services:
  discover:
    version: abc1234
    targets:
      - { type: cloud-run-job, name: discover-products, command: [] }
`)
	if err == nil || !strings.Contains(err.Error(), "`command` is empty") {
		t.Errorf("err = %v, want a refusal naming the empty command", err)
	}
}
