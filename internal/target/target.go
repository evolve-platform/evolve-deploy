// Package target defines what a cloud driver must implement.
//
// The split between Plan and Apply is the tool's central rule: everything that
// can fail cheaply — reading config, resolving references, checking that an
// image exists — happens for every target before any target is touched. A typo
// in one reference must not leave half a release deployed.
package target

import (
	"context"
	"fmt"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
)

// Capability reports which reference kinds a target type can be handed
// untouched, letting the platform resolve them when the container starts.
//
// Where a capability is false the tool has to read the value and write it as a
// literal, which is only allowed when refs.resolve is allow. Lambda is the case
// that forces this to exist: its environment variables are literal strings with
// no reference mechanism at all.
//
// envFrom has no capability flag: a bulk reference is always expanded during
// planning. It must point at a param store, never a secret store, so expanding
// it never means reading a secret. ECS environmentFiles and Kubernetes
// configMapRef could carry it untouched, but that would only change who does
// the reading, not what is read.
type Capability struct {
	NativeParam  bool
	NativeSecret bool
}

// EnvVar is one entry of a target's environment after planning. Value is either
// a literal (written as-is) or a reference (handed to the platform).
type EnvVar struct {
	Name  string
	Value refs.Value
}

// Desired is what a target should look like after the deploy.
type Desired struct {
	Service string
	Version string
	Target  *config.Target
	Env     []EnvVar

	// ManageEnv is false when the config says nothing about environment
	// variables, and the driver must then leave the running environment exactly
	// as it found it. Writing an empty list instead would delete everything
	// Terraform put there — which is the opposite of what a config that
	// mentions no variables is asking for.
	ManageEnv bool
}

// Change is a planned modification. Reason is shown to the user: a deploy that
// happens without the config changing — because Terraform moved the ECS base
// revision, say — is confusing unless the tool says why.
type Change struct {
	Service string
	Target  *config.Target

	FromVersion string
	ToVersion   string
	Reason      string

	EnvAdded   []string
	EnvChanged []string
	EnvRemoved []string

	// Payload is driver-private state carried from Plan to Apply, so Apply does
	// not have to read the world a second time and risk acting on state that
	// changed in between.
	Payload any
}

// Driver talks to one cloud.
type Driver interface {
	// Name is the cloud, for error messages.
	Name() string

	// Capabilities reports what the given target type can do natively.
	Capabilities(t config.TargetType) Capability

	// Verify checks that the tool is pointed at the destination the config
	// names. On AWS that is a real guard — the account is implicit in the
	// credentials, so a misconfigured CI variable would otherwise deploy a test
	// release to production.
	Verify(ctx context.Context) error

	// Resolver reads this cloud's parameter and secret stores.
	Resolver() refs.Resolver

	// Plan reads current state, checks that the artifact for Version exists,
	// and returns the change needed. A nil Change means nothing to do.
	Plan(ctx context.Context, d *Desired) (*Change, error)

	// Artifacts reports which of versions have a deployable artifact for this
	// target, keeping the order they were given in.
	//
	// This is the question `update` asks and planning does not: not "is this one
	// version there" but "which of these thirty could I pick". A version whose
	// build job failed, or whose commit never touched this service, has no
	// image — so without this the list would offer versions that cannot be
	// deployed, and the mistake would only surface at the next apply.
	//
	// Drivers that cannot list what exists for a target return
	// ErrArtifactsUnknown, which is a missing list and not a failure.
	Artifacts(ctx context.Context, t *config.Target, versions []string) ([]string, error)

	// Apply executes the change and waits until the target is healthy. On
	// failure it restores the previous version where the platform does not do
	// that itself, and returns an error either way.
	Apply(ctx context.Context, ch *Change) error

	// Revert undoes a change that already succeeded. It is called when a
	// sibling target of the same service failed: a service and its jobs run
	// the same image and may have a contract between them, so they move
	// together or not at all.
	Revert(ctx context.Context, ch *Change) error
}

// ErrNotImplemented is returned by drivers for clouds that are designed but not
// built yet, so the failure is explicit rather than a silent no-op.
type ErrNotImplemented struct {
	Cloud string
	Type  config.TargetType
}

func (e *ErrNotImplemented) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("%s: target type %q is not implemented yet", e.Cloud, e.Type)
	}
	return fmt.Sprintf("%s is not implemented yet", e.Cloud)
}
