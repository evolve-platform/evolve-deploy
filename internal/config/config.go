// Package config parses and validates deploy/<env>.yaml.
//
// The file is a deployment lockfile: it names every deployable, the version it
// should run, and where that lives. Parsing is strict — an unknown key is an
// error, not a warning — because a typo in a field name would otherwise silently
// deploy the wrong thing.
package config

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Cloud is the platform a config file targets. One cloud per repo.
type Cloud string

const (
	CloudAWS        Cloud = "aws"
	CloudGCP        Cloud = "gcp"
	CloudAzure      Cloud = "azure"
	CloudKubernetes Cloud = "kubernetes"
)

// TargetType selects which API a deployable is updated through.
type TargetType string

const (
	TypeECS          TargetType = "ecs"
	TypeLambda       TargetType = "lambda"
	TypeCloudRun     TargetType = "cloud-run"
	TypeContainerApp TargetType = "container-app"
	TypeContainerJob TargetType = "container-app-job"
	TypeFunctionApp  TargetType = "function-app"
	TypeHelm         TargetType = "helm"
)

// typesByCloud is also the validation table: a type is valid only for its cloud.
var typesByCloud = map[Cloud][]TargetType{
	CloudAWS:        {TypeECS, TypeLambda},
	CloudGCP:        {TypeCloudRun},
	CloudAzure:      {TypeContainerApp, TypeContainerJob, TypeFunctionApp},
	CloudKubernetes: {TypeHelm},
}

// StrategyType selects how a new version starts serving traffic.
type StrategyType string

const (
	// StrategyDirect writes the new version and lets the platform route to it
	// as soon as it is ready. This is today's behaviour and the default.
	//
	// It is not called "rolling" because on Container Apps and Cloud Run
	// nothing rolls — one revision replaces another whole — and only ECS does a
	// rolling update underneath.
	StrategyDirect StrategyType = "direct"

	// StrategyBlueGreen stages the new version on the side that carries no
	// traffic, runs the smoke commands against it, and then switches in one
	// write. The previous version stays running behind it, so a rollback is one
	// write and no container start.
	StrategyBlueGreen StrategyType = "blue-green"
)

// DefaultLabels name the two sides. Which of them carries the new version is
// fixed nowhere and does not matter: it is whichever one has no traffic.
var DefaultLabels = []string{"blue", "green"}

// Strategy is the shape of a release. It may be set once at file level and
// overridden per service, field by field.
type Strategy struct {
	Type StrategyType `yaml:"type"`

	// Smoke gates the whole release and is valid at file level only.
	//
	// It covers a deploy, not a service. Running the same suite once per
	// service would run it fourteen times for one release, and the test worth
	// running is usually a request through a router that touches several
	// services at once — which belongs to none of them. So there is one set of
	// commands, run once, after everything has staged and before anything
	// switches.
	//
	// A check of one revision on its own has not been lost: it is a line in
	// that same set, addressed by name — `curl -fsS {{url "purchase"}}/healthz`.
	Smoke []string `yaml:"smoke"`

	// Labels are the two side names. They do nothing functionally — the tool
	// reads them to know which traffic entries are its own, and to name them in
	// output — and exist only for a repository where Terraform bootstrapped
	// something other than blue and green.
	Labels []string `yaml:"labels"`

	// Env is the environment a revision gets for the side it is staged on,
	// keyed by label. It is how a staged side addresses its own downstream: the
	// router on the green side reads the green graph, the storefront on the
	// green side calls the green router, and nothing has to propagate a header
	// for that to hold.
	//
	// Additive, and deliberately not the same thing as a service's `env`: that
	// one means the deploy owns the whole environment, while this writes the
	// variables named here over whatever the revision inherited and leaves the
	// rest to Terraform. A side that names a variable the other does not is an
	// error rather than a merge, because the staged containers are copied from
	// the serving revision and the other side's value would come along.
	Env map[string]map[string]string `yaml:"env"`

	// KeepWarm leaves the previous version running at 0% traffic after the
	// switch, instead of switching it off.
	//
	// A Container Apps revision that is not deactivated keeps its own scale
	// rules, so with `minReplicas >= 1` the side nobody is using goes on
	// costing money between releases. Off by default for that reason: the
	// previous revision is deactivated once the new one serves, and a rollback
	// starts it again.
	//
	// It is a pointer so a service can turn it off over a file that turned it
	// on. What it buys is a rollback that is one write and no container start,
	// which is worth paying for on the environment where an outage is measured
	// in money and not on the four where it is not.
	KeepWarm *bool `yaml:"keep_warm"`

	// BakeTime is how long ECS keeps the old side up after the switch, and it
	// is the ECS answer to the question keep_warm asks everywhere else.
	//
	// ECS terminates the previous version itself once this is up — CLEAN_UP is
	// the platform's, not the tool's — so there the old side is never a
	// standing cost and never a standing rollback either. It is a window. Zero,
	// the default, means the switch is the end of it.
	//
	// A duration as Go writes them: 10m, 1h.
	BakeTime string `yaml:"bake_time"`
}

// SideEnvNames is every variable named by any side, which is the set the tool
// writes. A variable is set from the staged side when that side declares it, and
// the validation below makes sure both sides always do.
func (s *Strategy) SideEnvNames() []string {
	if s == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, vars := range s.Env {
		for name := range vars {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ReleaseName is what a staged release is called where a service name would
// otherwise go. Parenthesised so it reads as not-a-service, and reserved so it
// cannot be one.
const ReleaseName = "(release)"

// SideEnvVar is the name the tool writes the side into. Declared here so the
// config can reject a hand-written one; the reasoning lives on
// target.SideEnvVar.
const SideEnvVar = "EVOLVE_DEPLOY_SIDE"

// IsBlueGreen is the question every caller actually has.
func (s *Strategy) IsBlueGreen() bool { return s != nil && s.Type == StrategyBlueGreen }

// KeepsWarm reports whether the previous version stays running after a switch.
func (s *Strategy) KeepsWarm() bool { return s != nil && s.KeepWarm != nil && *s.KeepWarm }

// Bake is the parsed BakeTime, zero when unset. Validation has already refused
// anything that does not parse, so the error is dropped here.
func (s *Strategy) Bake() time.Duration {
	if s == nil || s.BakeTime == "" {
		return 0
	}
	d, _ := time.ParseDuration(s.BakeTime)
	return d
}

// merge layers a service's block over the file's.
//
// Lists are replaced whole rather than appended. A service that wants no smoke
// test has to be able to say so without repeating everything else, and
// appending would make that impossible to express.
func (s *Strategy) merge(over *Strategy) *Strategy {
	var out Strategy
	if s != nil {
		out = *s
	}
	if over != nil {
		if over.Type != "" {
			out.Type = over.Type
		}
		if over.Labels != nil {
			out.Labels = over.Labels
		}
		// Replaced whole, like the lists: a service that addresses its own
		// downstream by side has nothing in common with the file's default, and
		// half-inheriting a map of URLs is never what was meant.
		if over.Env != nil {
			out.Env = over.Env
		}
		// A pointer precisely so this can go the other way: a file that keeps
		// every side warm and one service that is not worth it.
		if over.KeepWarm != nil {
			out.KeepWarm = over.KeepWarm
		}
		if over.BakeTime != "" {
			out.BakeTime = over.BakeTime
		}
	}
	return &out
}

// ResolvePolicy controls what happens when a ref cannot be passed through to the
// platform natively and would have to be read by the tool instead.
type ResolvePolicy string

const (
	// ResolveAllow reads the value and writes it as a literal. Required for
	// Lambda, whose environment variables are literal strings with no reference
	// mechanism.
	ResolveAllow ResolvePolicy = "allow"
	// ResolveDeny fails the plan instead. Use it when no secret value may pass
	// through CI, at the cost of not being able to deploy Lambda targets that
	// carry refs.
	ResolveDeny ResolvePolicy = "deny"
)

// File is one deploy/<env>.yaml.
type File struct {
	Cloud CloudConfig `yaml:"cloud"`

	Refs RefConfig `yaml:"refs"`

	// Strategy is the default shape of a release for every service in this
	// file. A service may override it field by field.
	Strategy *Strategy `yaml:"strategy"`

	Services map[string]*Service `yaml:"services"`

	// Env is the environment name (tst/acc/prd), taken from the filename rather
	// than the contents so the two can never disagree. Available in refs and
	// hooks as ${env} / {{.env}}.
	Env string `yaml:"-"`
	// Path is where this file was read from, for error messages.
	Path string `yaml:"-"`
}

// CloudConfig says where a deploy goes.
//
// A tagged union: Provider selects which of the fields below apply and the rest
// are rejected, so the shape of the file is the same whichever cloud it targets
// and adding a cloud does not change it.
//
// On GCP, Azure and Kubernetes the addressing is a parameter on every API call,
// so naming it here is simply the address. On AWS the account is implicit in
// the credentials, which makes Account a guard: the tool compares it against
// sts:GetCallerIdentity and refuses on a mismatch. Without that, the only thing
// about this file that is not reviewable is where it points.
type CloudConfig struct {
	Provider Cloud `yaml:"provider"`

	// aws
	Account string `yaml:"account"`
	// aws, gcp
	Region string `yaml:"region"`
	// gcp
	Project string `yaml:"project"`

	// azure
	Subscription  string `yaml:"subscription"`
	ResourceGroup string `yaml:"resource_group"`
	// AppConfig is the App Configuration endpoint. Optional: it is needed only
	// when a ${param:…} reference is used, and most repositories keep their
	// setup values on the resource itself.
	AppConfig string `yaml:"app_config"`

	// kubernetes
	Context   string `yaml:"context"`
	Namespace string `yaml:"namespace"`
}
type RefConfig struct {
	Resolve ResolvePolicy `yaml:"resolve"`
}

// Service is one unit of release: a single version, one or more deployables.
//
// Keeping the version at service level means it is written once and cannot
// drift between a service and its jobs, which all run the same image.
type Service struct {
	Version string     `yaml:"version"`
	Type    TargetType `yaml:"type"`
	Targets []*Target  `yaml:"targets"`

	// Applied to every target of this service. A target may add to or
	// override Env.
	Env     map[string]string `yaml:"env"`
	EnvFrom []string          `yaml:"envFrom"`

	// Defaults for the targets below. Each is inherited only by the target
	// types it applies to, so a service with both an ECS service and a Lambda
	// can set `cluster` once without the Lambda picking up a field that means
	// nothing to it.
	Cluster   string `yaml:"cluster"`
	Base      string `yaml:"base"`
	Container string `yaml:"container"`
	Code      *Code  `yaml:"code"`

	// Strategy overrides the file's block for this service, field by field.
	// After normalisation it is the effective strategy and is never nil.
	Strategy *Strategy `yaml:"strategy"`

	// Hooks run once per service, not per target — publishing a schema five
	// times for a service with four jobs is not what anyone wants.
	Before []string `yaml:"before"`
	After  []string `yaml:"after"`

	// DependsOn names services that must finish before this one starts. For a
	// frontend that calls a backend, deploying both at once means the window
	// where the new frontend talks to the old backend is real but invisible.
	//
	// It is an ordering constraint and nothing more: it does not pull a
	// service into a release that is not otherwise part of it.
	DependsOn []string `yaml:"depends_on"`

	Name string `yaml:"-"`

	// strategyRaw is what this service's own block said, kept because
	// normalisation replaces Strategy with the merged result and validation
	// still needs to see what was actually written where.
	strategyRaw *Strategy
}

// Target is a single deployable resource.
type Target struct {
	Type TargetType        `yaml:"type"`
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`

	// Base is the task definition family Terraform owns, ECS only.
	//
	// ECS keeps image, env, cpu and healthcheck in one immutable
	// container_definitions blob, so field-level ignore_changes is impossible.
	// Two families give each owner its own object instead: Terraform registers
	// the shape into this one, nothing points at it, and the running family is
	// derived from it. Defaults to <name>-base.
	Base string `yaml:"base"`
	// Cluster is the ECS cluster name or ARN. Required for ecs targets.
	Cluster string `yaml:"cluster"`
	// Container names the container that carries the application image. Leave it
	// empty unless the task has several containers and none uses the
	// conventional name — see target.PickContainer. Sidecars such as a reverse
	// proxy or an OpenTelemetry collector are never touched.
	Container string `yaml:"container"`

	// TestURL is the address the test listener rule answers on, ECS only.
	//
	// Everywhere else the staged side has an address the tool can work out — a
	// label FQDN on Container Apps, a tagged URI from the Cloud Run API. ECS
	// routes test traffic through a listener rule, and a rule is not an
	// address: it may match on a host, a port or a header, and only the first
	// two are reachable by URL at all. So on ECS it is written down, and a
	// blue-green ECS target without one is refused — a gate nobody can address
	// is the whole point missing.
	TestURL string `yaml:"test_url"`

	// Code locates the deployment package for lambda and function-app targets.
	// Neither has a registry to read a tag from, so the location has to be
	// spelled out; {{.version}} is substituted.
	Code *Code `yaml:"code"`

	// Service is set during normalisation so a target can report where it
	// came from without a back-pointer to the whole service.
	Service string `yaml:"-"`

	// Strategy is the service's effective strategy, copied here during
	// normalisation. A driver is handed targets, not services, and it needs the
	// label names to know which traffic entries are the tool's own.
	Strategy *Strategy `yaml:"-"`

	// ManagesEnv records whether the config said anything about environment
	// variables at all.
	//
	// It is the difference between "leave the environment alone" and "the
	// environment is now empty". A config with no env and no envFrom is the
	// image-only mode: Terraform owns every variable and the deploy must not
	// touch them. Deriving this from the merged map would not work — a config
	// that declares nothing and one that declares nothing useful both end up
	// with an empty map.
	ManagesEnv bool `yaml:"-"`
}

// Label is how a target is named in output: "container-app/evolve-tst-site".
// It lives here so the plan and the progress lines cannot drift apart.
func (t *Target) Label() string { return string(t.Type) + "/" + t.Name }

// Code is where a deployment package lives, for the targets that ship a zip
// rather than an image.
type Code struct {
	// Bucket and Key locate a Lambda package in S3. Key may contain
	// {{.version}}, e.g. purchase-sha-{{.version}}.zip.
	Bucket string `yaml:"bucket"`
	Key    string `yaml:"key"`

	// URL locates an Azure Function App package in blob storage, and may
	// contain {{.version}} the same way. It is the full blob URL because that
	// is how Terraform already expresses it.
	URL string `yaml:"url"`
}

// Load reads and validates a config file. env is the environment name, normally
// derived from the filename by LoadEnv.
func Load(path, env string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	// Strict: an unrecognised key is a typo, and a silently ignored typo in a
	// deploy config is how you ship the wrong thing.
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	f.Path = path
	f.Env = env
	f.normalise()

	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// normalise fills in everything the rest of the tool may assume is present:
// every service has a name, and every service has at least one target with
// its env fully merged.
func (f *File) normalise() {
	if f.Refs.Resolve == "" {
		f.Refs.Resolve = ResolveAllow
	}

	for name, c := range f.Services {
		if c == nil {
			continue
		}
		c.Name = name

		// The effective strategy is the file's block with this service's laid
		// over it, so the rest of the tool reads one place and never nil.
		c.strategyRaw = c.Strategy
		c.Strategy = f.Strategy.merge(c.Strategy)
		if c.Strategy.Type == "" {
			c.Strategy.Type = StrategyDirect
		}
		if c.Strategy.Labels == nil {
			c.Strategy.Labels = slices.Clone(DefaultLabels)
		}

		// Short form: `type: ecs` means one target named after the service.
		// That covers the large majority of services; only those with jobs or a
		// sidecar lambda need to spell out a list.
		if len(c.Targets) == 0 && c.Type != "" {
			c.Targets = []*Target{{Type: c.Type, Name: name}}
		}

		for _, t := range c.Targets {
			t.Service = name
			t.Strategy = c.Strategy
			if t.Name == "" {
				t.Name = name
			}
			t.ManagesEnv = c.Env != nil || len(c.EnvFrom) > 0 || t.Env != nil
			t.Env = mergeEnv(c.Env, t.Env)

			switch t.Type {
			case TypeECS:
				if t.Cluster == "" {
					t.Cluster = c.Cluster
				}
				if t.Base == "" {
					t.Base = c.Base
				}
				if t.Container == "" {
					t.Container = c.Container
				}
				// Default last, so an inherited value still wins over it.
				if t.Base == "" {
					t.Base = t.Name + "-base"
				}
			case TypeLambda, TypeFunctionApp:
				if t.Code == nil {
					t.Code = c.Code
				}
			}
		}
	}
}

// mergeEnv layers target env over service env. Neither input is modified.
func mergeEnv(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	maps.Copy(out, base)
	maps.Copy(out, over)
	return out
}

func (f *File) validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	valid, ok := typesByCloud[f.Cloud.Provider]
	if !ok {
		add("cloud.provider: %q is not one of aws, gcp, azure, kubernetes", f.Cloud.Provider)
	}

	switch f.Refs.Resolve {
	case ResolveAllow, ResolveDeny:
	default:
		add("refs.resolve: %q is not one of allow, deny", f.Refs.Resolve)
	}

	for _, msg := range f.validateAddressing() {
		add("%s", msg)
	}

	for _, msg := range f.validateDependencies() {
		add("%s", msg)
	}

	for _, msg := range validateStrategy("strategy", f.Strategy, true) {
		add("%s", msg)
	}

	if len(f.Services) == 0 {
		add("services: no services defined")
	}

	for _, name := range f.ServiceNames() {
		c := f.Services[name]
		if c == nil {
			add("services.%s: empty", name)
			continue
		}
		if c.Version == "" {
			add("services.%s: version is required", name)
		}
		// The name a staged release is reported under, so a service cannot take
		// it: the failure list would name two different things the same.
		if name == ReleaseName {
			add("services.%s: %q is reserved — it is what the staged release is called",
				name, ReleaseName)
		}
		if c.Type == "" && len(c.Targets) == 0 {
			add("services.%s: needs either `type` (one target) or `targets`", name)
		}
		if c.Type != "" && len(c.Targets) > 1 {
			add("services.%s: set either `type` or `targets`, not both", name)
		}

		for _, msg := range validateStrategy(
			fmt.Sprintf("services.%s.strategy", name), c.strategyRaw, false) {
			add("%s", msg)
		}

		seen := map[string]bool{}
		for i, t := range c.Targets {
			where := fmt.Sprintf("services.%s.targets[%d]", name, i)
			if t.Type == "" {
				add("%s: type is required", where)
			} else if ok && !slicesContain(valid, t.Type) {
				add("%s: type %q is not valid on %s (want one of %s)",
					where, t.Type, f.Cloud.Provider, joinTypes(valid))
			}
			if t.Name == "" {
				add("%s: name is required", where)
			}
			if seen[string(t.Type)+"/"+t.Name] {
				add("%s: duplicate target %s/%s", where, t.Type, t.Name)
			}
			seen[string(t.Type)+"/"+t.Name] = true

			// Fields that belong to one target type only. Rejecting them
			// elsewhere catches copy-paste between entries, which is how a
			// target ends up silently ignoring half its configuration.
			if t.Type != TypeECS {
				if t.Base != "" {
					add("%s: `base` only applies to ecs targets", where)
				}
				if t.Cluster != "" {
					add("%s: `cluster` only applies to ecs targets", where)
				}
				if t.Container != "" {
					add("%s: `container` only applies to ecs targets", where)
				}
			}
			if t.Type != TypeLambda && t.Type != TypeFunctionApp && t.Code != nil {
				add("%s: `code` only applies to lambda and function-app targets", where)
			}

			// The tool writes this one itself on a blue-green target, and the
			// environment diff ignores it on purpose — so a config that also
			// set it would be changing something nothing would ever report.
			if _, ok := t.Env[SideEnvVar]; ok {
				add("%s: env.%s is written by the tool and cannot be set here", where, SideEnvVar)
			}

			switch t.Type {
			case TypeECS:
				if t.Cluster == "" {
					add("%s: cluster is required for ecs targets", where)
				}
			case TypeLambda:
				if t.Code == nil {
					add("%s: lambda targets need `code` with bucket and key", where)
					break
				}
				if t.Code.Bucket == "" {
					add("%s: code.bucket is required", where)
				}
				if t.Code.Key == "" {
					add("%s: code.key is required", where)
				}
			case TypeFunctionApp:
				// code.url is optional: a function app that runs a container
				// has no package, and which of the two it is can only be seen
				// on the resource. The driver reports it if the app needs one.
				if t.Code != nil && t.Code.Bucket != "" {
					add("%s: `code.bucket` is for lambda; function apps use `code.url`", where)
				}
			}
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("%s is invalid:\n  - %s", f.Path, strings.Join(errs, "\n  - "))
	}
	return nil
}

// validateStrategy checks one strategy block as it was written.
//
// Both the file's block and a service's own are checked, rather than the merged
// result: a bad value at file level would otherwise be reported once per
// service, and a block that contradicts itself is only visible before the merge
// flattens it.
func validateStrategy(where string, s *Strategy, fileLevel bool) []string {
	if s == nil {
		return nil
	}

	var msgs []string

	// The gate covers a release, so there is nowhere for a per-service one to
	// go. Rejecting it rather than merging it keeps `smoke` meaning one thing.
	if !fileLevel && s.Smoke != nil {
		msgs = append(msgs, fmt.Sprintf(
			"%s.smoke: gates the whole release, so it belongs in the file's "+
				"`strategy` block rather than on one service", where))
	}
	switch s.Type {
	case "", StrategyDirect, StrategyBlueGreen:
	default:
		msgs = append(msgs, fmt.Sprintf(
			"%s.type: %q is not one of direct, blue-green", where, s.Type))
	}

	// Smoke commands run against a staged side, and a direct release never
	// stages one. Written together in one block that is a contradiction, not an
	// inherited default that happens to go unused.
	if s.Type == StrategyDirect && len(s.Smoke) > 0 {
		msgs = append(msgs, fmt.Sprintf(
			"%s: `smoke` needs a staged side to run against, which `type: direct` never creates",
			where))
	}

	// Per-side environment needs sides to exist.
	if s.Type == StrategyDirect && len(s.Env) > 0 {
		msgs = append(msgs, fmt.Sprintf(
			"%s: `env` per side needs sides, which `type: direct` never has", where))
	}

	// Same again: there is no previous side to keep running when nothing is
	// staged beside the version that serves.
	if s.Type == StrategyDirect && s.KeepWarm != nil {
		msgs = append(msgs, fmt.Sprintf(
			"%s: `keep_warm` is about the side that stops serving, which `type: direct` never has",
			where))
	}
	if s.Type == StrategyDirect && s.BakeTime != "" {
		msgs = append(msgs, fmt.Sprintf(
			"%s: `bake_time` is the window before the old side is terminated, and "+
				"`type: direct` has no old side", where))
	}
	if s.BakeTime != "" {
		if d, err := time.ParseDuration(s.BakeTime); err != nil {
			msgs = append(msgs, fmt.Sprintf(
				"%s.bake_time: %q is not a duration (try 10m or 1h)", where, s.BakeTime))
		} else if d < 0 {
			msgs = append(msgs, fmt.Sprintf(
				"%s.bake_time: %s is negative", where, s.BakeTime))
		}
	}

	// Two names for one idea would be two ways to be wrong. keep_warm is a
	// standing state and bake_time is a window; a config that asks for both has
	// not decided which platform it is talking about.
	if s.KeepWarm != nil && s.BakeTime != "" {
		msgs = append(msgs, fmt.Sprintf(
			"%s: `keep_warm` and `bake_time` are the same question for different "+
				"platforms — keep_warm keeps the old side standing (Container Apps), "+
				"bake_time is how long before ECS terminates it. Set one.", where))
	}

	labels := s.Labels
	if labels == nil {
		labels = DefaultLabels
	}
	for side, vars := range s.Env {
		if !slices.Contains(labels, side) {
			msgs = append(msgs, fmt.Sprintf(
				"%s.env.%s: %q is not one of the sides (%s)",
				where, side, side, strings.Join(labels, ", ")))
			continue
		}
		if _, ok := vars[SideEnvVar]; ok {
			msgs = append(msgs, fmt.Sprintf(
				"%s.env.%s.%s: written by the tool and cannot be set here",
				where, side, SideEnvVar))
		}
	}

	// Every side has to name the same variables. The staged containers are
	// copied from the serving revision, so a variable only one side sets is not
	// "unset on the other" — it is the other side's value, silently inherited.
	// The tool could delete it instead, but a config that lists a downstream URL
	// for one side and not the other is a mistake either way, and saying so is
	// worth more than picking a behaviour for it.
	if len(s.Env) > 0 {
		for _, name := range s.SideEnvNames() {
			var missing []string
			for _, side := range labels {
				if _, ok := s.Env[side][name]; !ok {
					missing = append(missing, side)
				}
			}
			if len(missing) > 0 {
				msgs = append(msgs, fmt.Sprintf(
					"%s.env: %s is set for some sides but not %s — every side must name it",
					where, name, strings.Join(missing, " or ")))
			}
		}
	}

	if s.Labels != nil {
		switch {
		case len(s.Labels) != 2:
			msgs = append(msgs, fmt.Sprintf(
				"%s.labels: needs exactly two names, got %d", where, len(s.Labels)))
		case s.Labels[0] == "":
			msgs = append(msgs, fmt.Sprintf("%s.labels[0]: is empty", where))
		case s.Labels[1] == "":
			msgs = append(msgs, fmt.Sprintf("%s.labels[1]: is empty", where))
		case s.Labels[0] == s.Labels[1]:
			msgs = append(msgs, fmt.Sprintf(
				"%s.labels: both sides are called %q", where, s.Labels[0]))
		}
	}

	sort.Strings(msgs)
	return msgs
}

// validateAddressing checks that the fields naming the destination are present
// for this cloud, and that fields belonging to another cloud are absent. The
// second half matters: a leftover `project:` in an aws file means someone
// copied a file and did not finish editing it.
func (f *File) validateAddressing() []string {
	present := map[string]string{
		"account":        f.Cloud.Account,
		"region":         f.Cloud.Region,
		"project":        f.Cloud.Project,
		"subscription":   f.Cloud.Subscription,
		"resource_group": f.Cloud.ResourceGroup,
		"context":        f.Cloud.Context,
		"namespace":      f.Cloud.Namespace,
		"app_config":     f.Cloud.AppConfig,
	}

	// required must be set; optional may be. Anything else present belongs to
	// another cloud, which means someone copied a file and did not finish
	// editing it.
	var required, optional []string
	switch f.Cloud.Provider {
	case CloudAWS:
		required = []string{"account", "region"}
	case CloudGCP:
		required = []string{"project", "region"}
	case CloudAzure:
		required = []string{"subscription", "resource_group"}
		optional = []string{"app_config"}
	case CloudKubernetes:
		required = []string{"context", "namespace"}
	default:
		return nil
	}

	var msgs []string
	for _, key := range required {
		if present[key] == "" {
			msgs = append(msgs, fmt.Sprintf("cloud.%s: required when provider is %s", key, f.Cloud.Provider))
		}
	}
	for key, value := range present {
		if value == "" {
			continue
		}
		if !slicesContain(required, key) && !slicesContain(optional, key) {
			msgs = append(msgs, fmt.Sprintf("cloud.%s: does not apply when provider is %s", key, f.Cloud.Provider))
		}
	}
	sort.Strings(msgs)
	return msgs
}

// validateDependencies checks that every depends_on names a service in this
// file, and that the graph has no cycle.
//
// It also refuses an edge between two blue-green services, where ordering means
// nothing.
//
// All of it belongs here rather than at apply time: they are properties of the
// file, knowable by reading it, and a release must not get halfway through
// before discovering that two services are each waiting for the other.
func (f *File) validateDependencies() []string {
	var msgs []string

	for _, name := range f.ServiceNames() {
		c := f.Services[name]
		if c == nil {
			continue
		}
		for _, dep := range c.DependsOn {
			switch {
			case dep == name:
				msgs = append(msgs, fmt.Sprintf("services.%s: depends_on itself", name))
			case f.Services[dep] == nil:
				msgs = append(msgs, fmt.Sprintf(
					"services.%s: depends_on %q, which is not a service in this file", name, dep))

			// Between two staged services there is nothing left to order.
			// Staging carries no traffic, and a staged side reaches its own side
			// by label URL whatever the weights say, so the whole side is
			// complete and checked before any of it serves — that is the point
			// of releasing it as one. Waiting would only serialise the staging,
			// which is the slow part. An edge is refused rather than ignored
			// because a `depends_on` that quietly does nothing is worse than one
			// that is wrong.
			//
			// An edge with a `direct` service on either end still means
			// something: there is no other side to address there, so that one
			// really does go live before or after.
			case c.Strategy.IsBlueGreen() && f.Services[dep].Strategy.IsBlueGreen():
				msgs = append(msgs, fmt.Sprintf(
					"services.%s: depends_on %q, and both are blue-green — "+
						"the whole side is staged and checked before any of it serves, "+
						"so there is nothing to order",
					name, dep))
			}
		}
	}
	// Walking a graph that still has edges pointing at nothing reports
	// nonsense, so the names are settled first.
	if len(msgs) > 0 {
		return msgs
	}

	if cycle := f.findCycle(); len(cycle) > 0 {
		msgs = append(msgs, fmt.Sprintf(
			"services: depends_on forms a cycle: %s", strings.Join(cycle, " -> ")))
	}
	return msgs
}

// findCycle returns the services on a depends_on cycle, or nil.
//
// The path is the point: "a -> b -> c -> a" is something you can go and fix,
// where "there is a cycle" leaves you to find it yourself.
func (f *File) findCycle() []string {
	const (
		unvisited = iota
		onStack
		finished
	)

	state := make(map[string]int, len(f.Services))
	var path []string

	var walk func(name string) []string
	walk = func(name string) []string {
		c := f.Services[name]
		if c == nil {
			return nil
		}

		state[name] = onStack
		path = append(path, name)

		for _, dep := range c.DependsOn {
			switch state[dep] {
			case onStack:
				// Trimmed to where the loop closes, so the path shown is the
				// cycle itself rather than however we happened to reach it.
				for i, n := range path {
					if n == dep {
						return append(append([]string{}, path[i:]...), dep)
					}
				}
			case unvisited:
				if cycle := walk(dep); cycle != nil {
					return cycle
				}
			}
		}

		path = path[:len(path)-1]
		state[name] = finished
		return nil
	}

	for _, name := range f.ServiceNames() {
		if state[name] == unvisited {
			if cycle := walk(name); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// ServiceNames returns the service names in a stable order, so output and
// error messages do not shuffle between runs.
func (f *File) ServiceNames() []string {
	names := make([]string, 0, len(f.Services))
	for name := range f.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SelectServices narrows the file to the named services, erroring on any
// name that is not in it. An unknown name is a typo on the command line and
// should not silently deploy nothing.
func (f *File) SelectServices(only []string) error {
	if len(only) == 0 {
		return nil
	}
	keep := make(map[string]*Service, len(only))
	var unknown []string
	for _, name := range only {
		c, ok := f.Services[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		keep[name] = c
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s: no such service: %s", f.Path, strings.Join(unknown, ", "))
	}
	f.Services = keep
	return nil
}

// SetVersions overrides versions from --set name=version. Used on tst, where CI
// knows what it just built; acc and prd read the committed file instead.
func (f *File) SetVersions(overrides map[string]string) error {
	var unknown []string
	for name, version := range overrides {
		c, ok := f.Services[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		c.Version = version
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s: --set names unknown service: %s", f.Path, strings.Join(unknown, ", "))
	}
	return nil
}

func slicesContain[T comparable](haystack []T, needle T) bool {
	return slices.Contains(haystack, needle)
}

func joinTypes(types []TargetType) string {
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = string(t)
	}
	return strings.Join(out, ", ")
}
