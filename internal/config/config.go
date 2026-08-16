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

	Refs     RefConfig           `yaml:"refs"`
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

	// Hooks run once per service, not per target — publishing a schema five
	// times for a service with four jobs is not what anyone wants.
	Before []string `yaml:"before"`
	After  []string `yaml:"after"`

	Name string `yaml:"-"`
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

	// Code locates the deployment package for lambda and function-app targets.
	// Neither has a registry to read a tag from, so the location has to be
	// spelled out; {{.version}} is substituted.
	Code *Code `yaml:"code"`

	// Service is set during normalisation so a target can report where it
	// came from without a back-pointer to the whole service.
	Service string `yaml:"-"`

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

		// Short form: `type: ecs` means one target named after the service.
		// That covers the large majority of services; only those with jobs or a
		// sidecar lambda need to spell out a list.
		if len(c.Targets) == 0 && c.Type != "" {
			c.Targets = []*Target{{Type: c.Type, Name: name}}
		}

		for _, t := range c.Targets {
			t.Service = name
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
		if c.Type == "" && len(c.Targets) == 0 {
			add("services.%s: needs either `type` (one target) or `targets`", name)
		}
		if c.Type != "" && len(c.Targets) > 1 {
			add("services.%s: set either `type` or `targets`, not both", name)
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
