// Package target defines what a cloud driver must implement.
//
// The split between Plan and Apply is the tool's central rule: everything that
// can fail cheaply — reading config, resolving references, checking that an
// image exists — happens for every target before any target is touched. A typo
// in one reference must not leave half a release deployed.
package target

import (
	"context"
	"errors"
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

	// SideEnv is the resolved per-side environment, keyed by label. Written on
	// the staged side regardless of ManageEnv: these variables are how a side
	// addresses its own downstream, so they are added to the environment rather
	// than replacing it.
	SideEnv map[string][]EnvVar
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

	// Sides is set for a blue-green target: which label this change is going to
	// and which one it is coming from. Nil for a direct change, and nil for a
	// target that rides along without carrying traffic.
	Sides *Sides

	// Fallback is Rollout.Fallback for this target, filled in while planning so
	// the plan can say what a rollback would find without holding a driver.
	Fallback string

	// Carry marks a target that is staged only to keep the side complete:
	// nothing about it changed, and it is here because the side is a property of
	// the environment rather than of one service. A side missing an app is not a
	// stack, so it cannot be smoke-tested as one — and the label has to point at
	// something, since a revision can carry only one label and the serving one
	// already has the other.
	//
	// Whether these are deployed at all is decided for the release, not per
	// service: if nothing in the release has a real change, every carry is
	// dropped and the run does nothing. That is what keeps a second `apply` a
	// no-op.
	Carry bool

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

// SideEnvVar carries which side a revision is, into the revision itself.
//
// A service behind a router cannot be reached by tagging a request: a header
// only arrives if the router, the reverse proxy and every sidecar in the path
// forward it, and one that drops it produces a smoke test that quietly checks
// the old version and passes. The side is a property of the deployment, not of
// the request, so it belongs in the environment — where a service can resolve
// its own downstream by it, with nothing to propagate.
//
// It is written on every blue-green target and is not optional. Opt-in would
// mean a service that needs it can forget to declare it, and the failure mode
// of that is silent.
const SideEnvVar = config.SideEnvVar

// Side is one of the two labels and what it currently points at.
type Side struct {
	// Label is the name in the traffic block: blue or green, unless the config
	// says otherwise.
	Label string
	// Revision is what the label points at, empty when the label does not
	// exist yet. The idle side of an app that has only ever been deployed once
	// is exactly that.
	Revision string
	// Version is the image tag on that revision, empty when there is none to
	// read.
	Version string
}

// Sides is the answer to "which one is active", read from the platform.
//
// There is no state and no marker: active is the label carrying 100% of the
// traffic. A split means someone is in here by hand or a previous run died, and
// there is then no active side to deploy away from — so reading this fails
// rather than guessing which of the two may be thrown away.
type Sides struct {
	// Active carries 100% of the traffic. Its Revision is where FromVersion and
	// the environment diff are read from — not the resource's own template,
	// which with two live revisions is the last one created and after a failed
	// deploy is the one that was thrown away.
	Active Side
	// Idle is the other label: where this release is going.
	Idle Side

	// PinNeeded is set when Active is expressed as "whatever is newest"
	// (latestRevision on Azure, LATEST on Cloud Run) rather than as a revision
	// name.
	//
	// That is a rule, not a reference, and it has to be turned into a fact
	// before anything is staged: while it stands, every new revision takes 100%
	// of the traffic the moment it exists — before the smoke test and before a
	// single weight is written.
	PinNeeded bool
}

// Staged is where a newly created revision can be reached, before it serves
// anything.
type Staged struct {
	Label    string
	Revision string
	// URL is the label's own address. It is what makes a smoke test possible at
	// all: a revision with no traffic is still reachable there.
	URL string
}

// Rollout is implemented by drivers that can route traffic by label.
//
// The choreography lives in package plan and is the same on every platform that
// has labels and weights; only the writes differ. A target type that cannot
// route is a plan-time error when the config asks for blue-green, never a
// silent direct update.
type Rollout interface {
	// Routable reports whether this target type carries traffic at all. A
	// container app job has no ingress: it rides along with the service it
	// shares an image with and is written at the switch.
	Routable(t config.TargetType) bool

	// Pointable reports whether traffic here can be moved to a side by name,
	// outside a release. It is what `rollback` and `traffic --to` need, and it
	// is not the same question as Routable.
	//
	// On a platform that owns the sides itself — ECS swaps its own target
	// groups and terminates the old tasks — traffic is carried but no side
	// stands still long enough to be named. Such a target is routable, because
	// a release does move its traffic, and not pointable, because nothing
	// outside a release can.
	Pointable(t config.TargetType) bool

	// Sides reads the current split. It fails when no single label has 100%.
	Sides(ctx context.Context, t *config.Target) (*Sides, error)

	// Stage creates the new revision, waits until it is ready, points the idle
	// label at it, and returns where it can be reached. Nothing serves from it
	// yet.
	Stage(ctx context.Context, ch *Change) (*Staged, error)

	// Switch hands the staged side 100% of the traffic, in one write.
	Switch(ctx context.Context, ch *Change) error

	// Abandon puts the traffic back where it was and deactivates the staged
	// revision. Called when staging or the smoke test fails, and the point of
	// the whole exercise: nothing that anyone saw has happened yet.
	Abandon(ctx context.Context, ch *Change) error

	// Settle restates the invariant — one revision running, everything else
	// deactivated — and is rewritten on every successful deploy even when it
	// already held, so a run that died halfway is tidied by the next release.
	//
	// The side that stopped serving keeps its label at 0%, because that is the
	// rollback target and it has to stay named. It does not keep its replicas:
	// a version nobody is using should not cost anything. Point starts it again
	// when the rollback comes. `strategy.keep_warm` leaves it running instead,
	// for the environment where a cold start is the more expensive half.
	//
	// A failure here is a warning, not a failed deploy: the traffic is on the
	// new version and removing a working version over a cleanup call is worse
	// than the leftover.
	Settle(ctx context.Context, ch *Change) error

	// Point puts 100% of the traffic on one label without staging anything. It
	// is the manual rollback, and the way out of a split someone made by hand.
	//
	// It starts the revision it is about to hand traffic to when that revision
	// is stopped, and waits for it, because after a settle that is the normal
	// state of the side being rolled back to.
	Point(ctx context.Context, t *config.Target, label string) error

	// Fallback describes in a word or two what a rollback would find, for the
	// line both the plan and the apply print.
	//
	// It belongs to the driver and not to the orchestrator because the answer
	// is genuinely different per platform, and all three are true: a Container
	// Apps revision is stopped and can be started again, a Cloud Run revision
	// scales itself to zero and needs nothing, and ECS has terminated the old
	// tasks and left nothing behind at all. Deciding this in package plan would
	// mean the choreography knowing which cloud it is on, which is the one
	// thing it does not.
	Fallback(t *config.Target) string

	// Tidy switches off every revision that is not serving, reading what that
	// is off the platform rather than from a release.
	//
	// It is the same invariant Settle restores, for the paths that move traffic
	// without staging anything. Without it a rollback leaves the side it moved
	// away from running: Point starts what it is about to serve, and nothing
	// would ever stop what it stopped serving.
	//
	// Like Settle, a failure here is a warning rather than a failed command.
	// The traffic moved, which is what was asked for.
	Tidy(ctx context.Context, t *config.Target) error

	// Traffic reads the split as it is, without judging it.
	//
	// Separate from Sides on purpose: Sides refuses to interpret a split, and
	// "show me what is going on" has to work precisely when something is
	// wrong.
	Traffic(ctx context.Context, t *config.Target) ([]TrafficEntry, error)
}

// ErrNoWindow is what Undo returns when there is nothing left to reverse.
//
// Not a failure: it is the normal state of a target that was deployed some time
// ago, and the caller turns it into advice rather than an error.
var ErrNoWindow = errors.New("the release has finished; there is nothing left to reverse")

// Undoer is implemented by drivers whose platform owns the rollout and offers a
// window to reverse it rather than a standing side to point at.
//
// It is the other half of Pointable. On Container Apps and Cloud Run the way
// back is Point: name the side, move the weights, and that stays true forever
// because the side stays there. On ECS the way back is Undo: while the platform
// still has the previous version running it can put the traffic back, and once
// it has cleaned that up the way back is a deploy. Two shapes, because the
// platforms genuinely differ — a driver implements whichever is true of it.
//
// The string is what happened, in a phrase fit for a line of output.
type Undoer interface {
	Undo(ctx context.Context, t *config.Target) (string, error)
}

// TrafficEntry is one row of a target's traffic split.
type TrafficEntry struct {
	Label    string
	Revision string
	// Version is the image tag on that revision, empty when it could not be
	// read. A diagnostic is more useful with it and must not fail without it.
	Version string
	Weight  int
	// Latest marks an entry that says "whatever is newest" rather than naming a
	// revision.
	Latest bool
}
