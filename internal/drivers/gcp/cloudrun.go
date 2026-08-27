package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// secretVersion is the version Cloud Run reads a referenced secret at. "latest"
// matches what the cloud-run-service module already writes, so a rotation takes
// effect on the next revision without a deploy config change.
const secretVersion = "latest"

// cloudRunContainer has no conventional name: the module leaves the single
// container unnamed, and Cloud Run only requires names once there are sidecars.
// PickContainer therefore falls back to "the only one".
const cloudRunContainer = ""

type payload struct {
	template *runpb.RevisionTemplate
	previous *runpb.RevisionTemplate
	etag     string
}

// planService works out the next revision of a Cloud Run service.
func (d *Driver) planService(ctx context.Context, want *target.Desired) (*target.Change, error) {
	t := want.Target

	svc, err := d.services.GetService(ctx, &runpb.GetServiceRequest{
		Name: d.serviceName(t.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("cloud run service %s: %w", t.Name, err)
	}
	if svc.GetTemplate() == nil {
		return nil, fmt.Errorf("cloud run service %s has no template", t.Name)
	}

	current := svc.GetTemplate()
	name, err := target.PickContainer(
		containerNames(current.GetContainers()), t.Container, cloudRunContainer)
	if err != nil {
		return nil, fmt.Errorf("cloud run service %s: %w", t.Name, err)
	}

	next, from, err := nextTemplate(current, name, want.Version, want.Env, want.ManageEnv)
	if err != nil {
		return nil, err
	}

	added, changed, removed := diffEnv(current.GetContainers(), next.GetContainers(), name, nil)
	if len(added)+len(changed)+len(removed) == 0 && from == want.Version {
		return nil, nil
	}

	return &target.Change{
		Service:     want.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   want.Version,
		Reason:      reason(from, want.Version),
		EnvAdded:    added,
		EnvChanged:  changed,
		EnvRemoved:  removed,
		PublicURL:   svc.GetUri(),
		Payload:     &payload{template: next, previous: current, etag: svc.GetEtag()},
	}, nil
}

func (d *Driver) applyService(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*payload)

	if err := d.update(ctx, ch.Target.Name, p.template, p.etag); err != nil {
		// Cloud Run only routes traffic to a revision once it is ready, so a
		// container that never starts does not take the service down. Putting
		// the previous template back is still worth doing: it stops the failed
		// revision from being the one a later deploy compares against.
		if rbErr := d.update(ctx, ch.Target.Name, p.previous, ""); rbErr != nil {
			return errors.Join(
				fmt.Errorf("rolling back to %s also failed", target.VersionOrUnknown(ch.FromVersion)),
				err, rbErr)
		}
		return fmt.Errorf("rolled back to %s: %w", target.VersionOrUnknown(ch.FromVersion), err)
	}
	return nil
}

func (d *Driver) revertService(ctx context.Context, ch *target.Change) error {
	// No etag on a revert: the state has moved on since the plan by definition,
	// and refusing to undo because of that would be the wrong way round.
	return d.update(ctx, ch.Target.Name, ch.Payload.(*payload).previous, "")
}

// update writes the template and waits for the revision to be serving.
//
// The field mask limits the write to the template, so ingress, IAM, traffic
// split and labels are left exactly as Terraform set them. The long-running
// operation completes once the service has reconciled, which for Cloud Run
// means the new revision is ready — a container that fails to start makes the
// operation fail rather than silently leaving a broken revision behind.
func (d *Driver) update(ctx context.Context, name string, tmpl *runpb.RevisionTemplate, etag string) error {
	svc := &runpb.Service{
		Name:     d.serviceName(name),
		Template: tmpl,
		// An etag makes a concurrent write fail instead of overwriting it. Two
		// deploys racing on one service should not silently pick a winner.
		Etag: etag,
	}

	timer := logging.Start("update cloud run service", "name", name,
		"containers", len(tmpl.GetContainers()), "etag", etag != "")

	op, err := d.services.UpdateService(ctx, &runpb.UpdateServiceRequest{
		Service:    svc,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"template"}},
	})
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}

	done, err := op.Wait(ctx)
	if err != nil {
		return fmt.Errorf("waiting for %s: %w", name, err)
	}
	timer.Done("revision", done.GetLatestCreatedRevision(),
		"ready", done.GetLatestReadyRevision())

	// The operation succeeding is not quite the same as the service being
	// healthy, so the terminal condition is checked as well.
	if cond := done.GetTerminalCondition(); cond != nil &&
		cond.GetState() != runpb.Condition_CONDITION_SUCCEEDED &&
		cond.GetState() != runpb.Condition_STATE_UNSPECIFIED {
		return fmt.Errorf("%s is %s: %s", name, cond.GetState(), cond.GetMessage())
	}
	return nil
}

// nextTemplate copies the template and replaces the image tag of one container,
// and its environment when the config declares one. Sidecars are copied through:
// their images and settings are Terraform's.
func nextTemplate(
	current *runpb.RevisionTemplate,
	name, version string,
	env []target.EnvVar,
	manageEnv bool,
) (next *runpb.RevisionTemplate, from string, err error) {
	next = proto.Clone(current).(*runpb.RevisionTemplate)

	// A revision name is unique per revision, so carrying the old one over
	// would make Cloud Run reject the update.
	next.Revision = ""

	from, err = retag(next.GetContainers(), name, version, env, manageEnv)
	if err != nil {
		return nil, "", err
	}

	// The version, written where the revision keeps it, because the image will
	// not: Cloud Run stores a container image as the digest the tag resolved to,
	// so a revision read back names an image without naming a release. The
	// service's template does keep the tag, but with two live revisions it
	// belongs to whichever was created last — the abandoned one, after a failed
	// deploy — which is the whole reason a blue-green plan reads the serving
	// revision instead. This is the note that survives that read.
	//
	// An annotation rather than a label: a label value may not hold a dot or a
	// capital, and `v1.2.3` is a tag someone will use. Annotations are the field
	// the API sets aside for exactly this, and Cloud Run rejects only its own
	// reserved namespaces.
	if next.Annotations == nil {
		next.Annotations = map[string]string{}
	}
	next.Annotations[versionAnnotation] = version

	return next, from, nil
}

// versionAnnotation names the release a revision is running. Prefixed, because
// an unprefixed annotation key is the API's to give meaning to and this one is
// the tool's.
const versionAnnotation = "evolve-deploy/version"

// retag rewrites one container of an already-copied list in place: the image
// tag, and the environment when the config declares one.
//
// It works on containers rather than on a template because a service and a job
// hold theirs in different messages — a RevisionTemplate against a TaskTemplate
// two levels down a Job — while the rule for the container itself is the same
// on both. Everything not named here is left as it was found, which is how a
// sidecar keeps the image and settings Terraform gave it.
func retag(
	containers []*runpb.Container,
	name, version string,
	env []target.EnvVar,
	manageEnv bool,
) (from string, err error) {
	var found bool
	for _, c := range containers {
		if c.GetName() != name {
			continue
		}
		img, err := image.Retag(c.GetImage(), version)
		if err != nil {
			return "", err
		}
		from = image.Tag(c.GetImage())
		c.Image = img
		c.Env = containerEnv(c.GetEnv(), renderEnv(env), manageEnv)
		found = true
	}
	if !found {
		return "", fmt.Errorf("container %q disappeared while building the update", name)
	}
	return from, nil
}

// containerEnv is the environment the next revision carries for the container
// being released.
//
// The deploy config is the whole declaration, so a name it does not mention is
// gone. Cloud Run alone could have kept the merge -- a service is Terraform's to
// correct here, unlike an Azure container whose `env` is on `ignore_changes` --
// but then the same list of variables would mean the environment on one cloud
// and a patch over an unseen one on another, and no reader of a deploy file
// could tell which without knowing where it lands. Configuration that used to
// arrive by being left on the revision belongs in Secret Manager or a parameter
// store the service reads for itself.
//
// A config that declares no environment at all keeps what is there. That is not
// the same ambiguity: it is a repository that has not moved its environment here
// yet, and emptying one would be a poor way to tell it so.
//
// Only the released container is touched, so a sidecar's environment stays
// Terraform's. The side variable is written afterwards by withSide, so replacing
// here cannot drop it.
func containerEnv(current, declared []*runpb.EnvVar, manage bool) []*runpb.EnvVar {
	if !manage {
		return current
	}
	return declared
}

// renderEnv turns the planned environment into Cloud Run variables. A reference
// becomes a secretKeyRef, which Cloud Run resolves when the revision starts —
// so no secret value passes through this tool.
func renderEnv(env []target.EnvVar) []*runpb.EnvVar {
	out := make([]*runpb.EnvVar, 0, len(env))
	for _, e := range env {
		if e.Value.IsRef() {
			out = append(out, &runpb.EnvVar{
				Name: e.Name,
				Values: &runpb.EnvVar_ValueSource{
					ValueSource: &runpb.EnvVarSource{
						SecretKeyRef: &runpb.SecretKeySelector{
							Secret:  e.Value.Name,
							Version: secretVersion,
						},
					},
				},
			})
			continue
		}
		out = append(out, &runpb.EnvVar{
			Name:   e.Name,
			Values: &runpb.EnvVar_Value{Value: e.Value.Literal},
		})
	}
	return out
}

// envFingerprint flattens an environment for comparison. A reference is
// compared by what it points at: the tool cannot see the value, and a rotated
// secret is not a change it should claim to have made.
//
// `ignore` is what the tool writes per side. The side alternates every release,
// so the desired value never matches what the serving revision carries;
// comparing it would report a change on every run, which would deploy on every
// run, which would flip the sides forever with no version ever changing.
// Anything `strategy.env` sets per side is skipped for the same reason.
func envFingerprint(env []*runpb.EnvVar, ignore []string) map[string]string {
	skip := map[string]bool{target.SideEnvVar: true}
	for _, name := range ignore {
		skip[name] = true
	}

	out := make(map[string]string, len(env))
	for _, e := range env {
		if skip[e.GetName()] {
			continue
		}
		if src := e.GetValueSource().GetSecretKeyRef(); src != nil {
			out[e.GetName()] = "->" + src.GetSecret()
			continue
		}
		out[e.GetName()] = "=" + e.GetValue()
	}
	return out
}

func diffEnv(
	current, next []*runpb.Container,
	name string,
	ignore []string,
) (added, changed, removed []string) {
	have := envFingerprint(findContainer(current, name).GetEnv(), ignore)
	want := envFingerprint(findContainer(next, name).GetEnv(), ignore)

	for k, wv := range want {
		hv, ok := have[k]
		switch {
		case !ok:
			added = append(added, k)
		case hv != wv:
			changed = append(changed, k)
		}
	}
	for k := range have {
		if _, ok := want[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return added, changed, removed
}

func containerNames(containers []*runpb.Container) []string {
	out := make([]string, 0, len(containers))
	for _, c := range containers {
		out = append(out, c.GetName())
	}
	return out
}

func findContainer(containers []*runpb.Container, name string) *runpb.Container {
	for _, c := range containers {
		if c.GetName() == name {
			return c
		}
	}
	return nil
}

func reason(from, to string) string {
	if from != to {
		return fmt.Sprintf("version %s -> %s", target.VersionOrUnknown(from), to)
	}
	return "environment changed"
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func parseJSONMap(where, raw string) (map[string]string, error) {
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%s does not hold a JSON object of strings: %w", where, err)
	}
	return out, nil
}
