package azure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// weightAll and weightNone are the only two weights this tool writes. There is
// no gradual shift: a canary without metrics is a stopwatch, and there are no
// metrics here.
const (
	weightAll  int32 = 100
	weightNone int32 = 0
)

// bgPayload is what a blue-green change carries from Plan to Apply.
type bgPayload struct {
	// containers is the template for the new revision, built from the app's own
	// template so that what Terraform declares in the container reaches the
	// release — see planAppBlueGreen.
	containers []*armappcontainers.Container

	// fqdn is the app's ingress hostname, which the label URL is derived from.
	fqdn string

	// staged is filled in by Stage and read by Switch, Abandon and Settle.
	staged string
}

// Routable reports which target types carry traffic.
//
// A container app job has no ingress and nothing to split, and a function app
// has slots rather than labels. Both can still be part of a blue-green service:
// they ride along and are written at the switch.
func (d *Driver) Routable(t config.TargetType) bool {
	return t == config.TypeContainerApp
}

// Pointable: the traffic block is the tool's own, so a label can be handed
// everything at any time.
func (d *Driver) Pointable(t config.TargetType) bool { return d.Routable(t) }

// Fallback: the previous revision keeps its label either way, and the question
// is only whether it kept its replicas with it.
func (d *Driver) Fallback(t *config.Target) string {
	if t.Strategy.KeepsWarm() {
		return "warm"
	}
	return "stopped"
}

// Sides reads the current split off the app.
func (d *Driver) Sides(ctx context.Context, t *config.Target) (*target.Sides, error) {
	app, err := d.getApp(ctx, t.Name)
	if err != nil {
		return nil, err
	}
	return d.sidesOf(ctx, t, app)
}

func (d *Driver) sidesOf(
	ctx context.Context,
	t *config.Target,
	app *armappcontainers.ContainerApp,
) (*target.Sides, error) {
	cfg := app.Properties.Configuration
	if cfg == nil {
		return nil, fmt.Errorf("container app %s has no configuration", t.Name)
	}

	// Single-revision mode has no labels and nothing to divide. Terraform owns
	// this setting, so the tool reports it rather than changing it.
	if cfg.ActiveRevisionsMode == nil ||
		*cfg.ActiveRevisionsMode != armappcontainers.ActiveRevisionsModeMultiple {
		mode := "an unset"
		if cfg.ActiveRevisionsMode != nil {
			mode = string(*cfg.ActiveRevisionsMode)
		}
		return nil, fmt.Errorf(
			"container app %s is in %s revision mode; blue-green needs Multiple "+
				"(set revision_mode = \"Multiple\" in Terraform)", t.Name, mode)
	}
	if cfg.Ingress == nil {
		return nil, fmt.Errorf(
			"container app %s has no ingress, so it carries no traffic to divide", t.Name)
	}

	sides, err := readSides(cfg.Ingress.Traffic, t.Strategy.Labels)
	if err != nil {
		return nil, fmt.Errorf("container app %s: %w", t.Name, err)
	}

	// The version that is running is the tag on the revision that is serving,
	// not the tag in the app's template. With two live revisions the template
	// is whichever was created last, which after a failed deploy is the one
	// that was thrown away.
	if sides.Active.Revision != "" {
		if tmpl, err := d.revisionTemplate(ctx, t.Name, sides.Active.Revision); err == nil {
			name, err := target.PickContainer(
				containerNames(tmpl.Containers), t.Container, appContainer)
			if err == nil {
				if c := findContainer(tmpl.Containers, name); c != nil {
					sides.Active.Version = image.Tag(derefString(c.Image))
				}
			}
		} else {
			slog.Debug("could not read the serving revision", "app", t.Name,
				"revision", sides.Active.Revision, "err", err)
		}
	}
	return sides, nil
}

// readSides applies the one rule this whole feature rests on: active is the
// label carrying 100% of the traffic, and anything else is a refusal.
//
// A split means someone is in here by hand or a previous run died. There is
// then no active side to deploy away from, and carrying on would mean the tool
// deciding for itself which of the two revisions it may throw away.
func readSides(traffic []*armappcontainers.TrafficWeight, labels []string) (*target.Sides, error) {
	if len(labels) != 2 {
		return nil, fmt.Errorf("strategy.labels needs exactly two names, got %d", len(labels))
	}

	var (
		serving []*armappcontainers.TrafficWeight
		byLabel = map[string]*armappcontainers.TrafficWeight{}
	)
	for _, w := range traffic {
		if w == nil {
			continue
		}
		if derefInt32(w.Weight) > 0 {
			serving = append(serving, w)
		}
		if l := derefString(w.Label); l != "" {
			byLabel[l] = w
		}
	}

	if len(serving) != 1 || derefInt32(serving[0].Weight) != weightAll {
		return nil, fmt.Errorf(
			"traffic is split, so there is no active side:\n%s\n"+
				"    resolve it first with `evolve-deploy traffic <config> --to %s`",
			describeTraffic(traffic), labels[0])
	}

	active := serving[0]
	label := derefString(active.Label)
	if label == "" {
		// Nothing here can fix this — `traffic --to` moves a label that exists
		// and cannot invent the first one — so the refusal names where it comes
		// from instead of a command that would fail differently.
		return nil, fmt.Errorf(
			"100%% of the traffic goes to an entry with no label, so there is "+
				"nothing to fall back to:\n%s\n"+
				"    A first side has to be bootstrapped in Terraform: give the serving\n"+
				"    revision `label = \"%s\"` in the app's traffic_weight block, then leave\n"+
				"    the block to the tool with ignore_changes",
			describeTraffic(traffic), labels[0])
	}
	idx := slices.Index(labels, label)
	if idx < 0 {
		return nil, fmt.Errorf(
			"the label carrying all the traffic is %q, which is not one of %s:\n%s",
			label, strings.Join(labels, " or "), describeTraffic(traffic))
	}

	idleLabel := labels[1-idx]
	sides := &target.Sides{
		Active: target.Side{
			Label:    label,
			Revision: derefString(active.RevisionName),
		},
		// The idle label may not exist yet: an app that has only ever been
		// deployed once has one side and no other.
		Idle: target.Side{Label: idleLabel},
		// "Whatever is newest" is a rule, not a reference. While it stands,
		// every new revision takes all the traffic the moment it exists — so it
		// has to become a fact before anything is staged.
		PinNeeded: derefBool(active.LatestRevision),
	}
	if w, ok := byLabel[idleLabel]; ok {
		sides.Idle.Revision = derefString(w.RevisionName)
	}
	return sides, nil
}

// describeTraffic renders a traffic block for an error message. Whoever reads a
// refusal needs to see what was actually there.
func describeTraffic(traffic []*armappcontainers.TrafficWeight) string {
	if len(traffic) == 0 {
		return "      (the traffic block is empty)"
	}
	var lines []string
	for _, w := range traffic {
		if w == nil {
			continue
		}
		revision := derefString(w.RevisionName)
		if derefBool(w.LatestRevision) {
			revision = "(latest)"
		}
		lines = append(lines, fmt.Sprintf("      %-8s %-40s %d%%",
			orNone(derefString(w.Label)), orNone(revision), derefInt32(w.Weight)))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// weights builds a traffic block. A side with no revision is left out: a label
// cannot point at nothing.
func weights(entries ...trafficEntry) []*armappcontainers.TrafficWeight {
	out := make([]*armappcontainers.TrafficWeight, 0, len(entries))
	for _, e := range entries {
		if e.side.Revision == "" {
			continue
		}
		out = append(out, &armappcontainers.TrafficWeight{
			RevisionName: to.Ptr(e.side.Revision),
			Label:        to.Ptr(e.side.Label),
			Weight:       to.Ptr(e.weight),
		})
	}
	return out
}

type trafficEntry struct {
	side   target.Side
	weight int32
}

// labelURL turns the app's hostname into the label's own.
//
// A label FQDN is not returned by the API, so it is assembled: evolve-tst-site
// .<domain> becomes evolve-tst-site---green.<domain>. That address is what makes
// a smoke test possible at all — a revision with no traffic is still reachable
// through its label.
func labelURL(fqdn, label string) string {
	if fqdn == "" || label == "" {
		return ""
	}
	host, domain, ok := strings.Cut(fqdn, ".")
	if !ok {
		return ""
	}
	return "https://" + host + "---" + label + "." + domain
}

// getApp reads an app and checks the parts every caller needs are present.
func (d *Driver) getApp(ctx context.Context, name string) (*armappcontainers.ContainerApp, error) {
	timer := logging.Start("get container app", "name", name)
	got, err := d.apps.Get(ctx, d.file.Cloud.ResourceGroup, name, nil)
	if err != nil {
		return nil, fmt.Errorf("container app %s: %w", name, err)
	}
	timer.Done()

	if got.Properties == nil || got.Properties.Template == nil {
		return nil, fmt.Errorf("container app %s has no template", name)
	}
	return &got.ContainerApp, nil
}

// revisionTemplate reads one revision's own template.
//
// This is the read that makes blue-green correct rather than nearly correct.
// With two live revisions the app's template is whichever was created last —
// after a failed deploy, the one that was abandoned. Comparing against that
// would report "already up to date" for a version that never shipped.
func (d *Driver) revisionTemplate(
	ctx context.Context,
	app, revision string,
) (*armappcontainers.Template, error) {
	got, err := d.revisions.GetRevision(ctx, d.file.Cloud.ResourceGroup, app, revision, nil)
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision, err)
	}
	if got.Properties == nil || got.Properties.Template == nil {
		return nil, fmt.Errorf("revision %s has no template", revision)
	}
	return got.Properties.Template, nil
}

// Stage creates the new revision and points the idle label at it.
//
// The order of the writes is the whole story. While the active label says
// "whatever is newest", every revision created takes all the traffic the moment
// it exists — before the smoke test, before a weight is written. So the pin
// comes first, in a write of its own, and only then is a revision created.
func (d *Driver) Stage(ctx context.Context, ch *target.Change) (*target.Staged, error) {
	p := ch.Payload.(*bgPayload)
	name := ch.Target.Name

	if ch.Sides.PinNeeded {
		slog.Debug("pinning the active label before staging",
			"app", name, "label", ch.Sides.Active.Label,
			"revision", ch.Sides.Active.Revision)
		if err := d.patchTraffic(ctx, name, weights(
			trafficEntry{ch.Sides.Active, weightAll},
			trafficEntry{ch.Sides.Idle, weightNone},
		)); err != nil {
			return nil, fmt.Errorf("pinning %s before staging: %w", ch.Sides.Active.Label, err)
		}
	}

	if err := d.patchTemplate(ctx, name, p.containers); err != nil {
		return nil, err
	}

	// LatestRevisionName, not LatestReadyRevisionName.
	//
	// The latter is sticky: it names the newest revision that became ready at
	// some point, and keeps naming it after that revision has been deactivated.
	// A retry of a release whose gate failed patches the same template, which
	// Container Apps answers with the revision it already has — so latest and
	// latest-ready are both that dead revision, waitReady's comparison is true
	// on the first poll, and the label lands on something with no replicas. Which
	// is a smoke test timing out against a URL that resolves.
	app, err := d.getApp(ctx, name)
	if err != nil {
		return nil, err
	}
	staged := derefString(app.Properties.LatestRevisionName)
	if staged == "" {
		return nil, fmt.Errorf("container app %s reported no revision after staging", name)
	}
	p.staged = staged

	// An identical template creates no new revision, so this may be one an
	// earlier abandoned release switched off. Nothing else will turn it back on.
	if err := d.activateIfStopped(ctx, name, staged); err != nil {
		return nil, err
	}
	if err := d.waitRunning(ctx, name, staged, readyTimeout); err != nil {
		return nil, err
	}

	if err := d.patchTraffic(ctx, name, weights(
		trafficEntry{ch.Sides.Active, weightAll},
		trafficEntry{target.Side{Label: ch.Sides.Idle.Label, Revision: staged}, weightNone},
	)); err != nil {
		return nil, fmt.Errorf("attaching label %s to %s: %w", ch.Sides.Idle.Label, staged, err)
	}

	return &target.Staged{
		Label:    ch.Sides.Idle.Label,
		Revision: staged,
		URL:      labelURL(p.fqdn, ch.Sides.Idle.Label),
	}, nil
}

// Switch hands the staged side all of the traffic, in one write.
func (d *Driver) Switch(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*bgPayload)
	return d.patchTraffic(ctx, ch.Target.Name, weights(
		trafficEntry{target.Side{Label: ch.Sides.Idle.Label, Revision: p.staged}, weightAll},
		trafficEntry{ch.Sides.Active, weightNone},
	))
}

// Abandon restores the traffic block exactly as it was and deactivates the
// revision that never served anything.
//
// This is the path that makes a failed blue-green deploy a non-event: nothing
// anyone saw has happened, and the recovery is switching off a revision that
// handled no requests. Restoring the idle label to what it pointed at before
// matters — staging moved it, and leaving it on an abandoned revision would
// make the next release's rollback target a version that never worked.
func (d *Driver) Abandon(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*bgPayload)

	err := d.patchTraffic(ctx, ch.Target.Name, weights(
		trafficEntry{ch.Sides.Active, weightAll},
		trafficEntry{ch.Sides.Idle, weightNone},
	))

	// Only switch off a revision this release actually brought up. When an
	// identical template made Container Apps hand back the revision the idle
	// label already pointed at, deactivating it undoes something the release
	// never did — and leaves the label naming a revision with no replicas, which
	// is a URL that resolves and answers nothing.
	if p.staged != "" && p.staged != ch.Sides.Idle.Revision {
		// Deactivated even when the traffic write failed: the revision is
		// unwanted either way, and leaving it running costs money for nothing.
		if dErr := d.deactivate(ctx, ch.Target.Name, p.staged); dErr != nil && err == nil {
			err = dErr
		}
	}
	return err
}

// Settle switches off every revision that is not serving.
//
// The traffic half of the cleanup is already in place: Switch writes the
// canonical two-entry block, which also drops any leftover entry a run that
// died halfway left behind. So this is the deactivation, and repeating the
// write here would be a second ARM round trip that changes nothing.
//
// The previous version keeps its label at 0% — that is the rollback target and
// it stays named — but it does not keep its replicas. A Container Apps revision
// that is not deactivated holds on to its own scale rules, so an app with
// minReplicas >= 1 pays for both sides for as long as the pair stands. Rolling
// back therefore starts it again rather than finding it warm; `keep_warm` buys
// the old behaviour back where that trade is worth making.
func (d *Driver) Settle(ctx context.Context, ch *target.Change) error {
	return d.deactivateRest(ctx, ch.Target.Name, settleKeep(ch))
}

// settleKeep is the list of revisions a settle leaves running. Split out from
// Settle because the decision is worth testing and the ARM call is not.
func settleKeep(ch *target.Change) []string {
	p := ch.Payload.(*bgPayload)

	keep := []string{p.staged}
	if ch.Target.Strategy.KeepsWarm() {
		keep = append(keep, ch.Sides.Active.Revision)
	}
	return keep
}

// Point puts all the traffic on one label without staging anything.
//
// It reads the traffic block directly instead of going through Sides, and that
// is deliberate: the state this has to repair is exactly the state Sides
// refuses to interpret.
//
// The revision is started before the weight moves, because after a settle the
// side being rolled back to is deactivated. Handing traffic to it as it stands
// would be a URL that resolves and answers nothing — the same failure the
// staging path already guards against. Both calls are no-ops when it is already
// running, so escaping a split is no slower than it was.
func (d *Driver) Point(ctx context.Context, t *config.Target, label string) error {
	app, err := d.getApp(ctx, t.Name)
	if err != nil {
		return err
	}
	cfg := app.Properties.Configuration
	if cfg == nil || cfg.Ingress == nil {
		return fmt.Errorf("container app %s has no ingress", t.Name)
	}

	next, err := pointTraffic(cfg.Ingress.Traffic, label)
	if err != nil {
		return fmt.Errorf("container app %s: %w", t.Name, err)
	}

	if revision := servingRevision(next); revision != "" {
		if err := d.activateIfStopped(ctx, t.Name, revision); err != nil {
			return err
		}
		if err := d.waitRunning(ctx, t.Name, revision, readyTimeout); err != nil {
			return err
		}
	}
	return d.patchTraffic(ctx, t.Name, next)
}

// Tidy switches off everything that is not serving, for the paths that move
// traffic without staging anything.
//
// Settle can name the revision to keep because it just created it. Here there
// is no release to ask, so the traffic block is the source: whatever holds all
// of it is what stays running. That also means this refuses a split rather than
// picking a side — deactivating the wrong half of one is not a cleanup.
func (d *Driver) Tidy(ctx context.Context, t *config.Target) error {
	app, err := d.getApp(ctx, t.Name)
	if err != nil {
		return err
	}
	cfg := app.Properties.Configuration
	if cfg == nil || cfg.Ingress == nil {
		return fmt.Errorf("container app %s has no ingress", t.Name)
	}

	keep := tidyKeep(cfg.Ingress.Traffic, t.Strategy.KeepsWarm())
	if len(keep) == 0 {
		return fmt.Errorf(
			"container app %s: no single revision carries all the traffic, so there is "+
				"nothing to tidy around:\n%s", t.Name, describeTraffic(cfg.Ingress.Traffic))
	}
	return d.deactivateRest(ctx, t.Name, keep)
}

// tidyKeep is the list of revisions a tidy leaves running, read off the traffic
// block. Empty when nothing carries all of it, which is a refusal rather than a
// list: deactivating the wrong half of a split is not a cleanup.
func tidyKeep(traffic []*armappcontainers.TrafficWeight, keepWarm bool) []string {
	serving := servingRevision(traffic)
	if serving == "" {
		return nil
	}

	keep := []string{serving}
	if !keepWarm {
		return keep
	}

	// The other labelled revision is the rollback target and is meant to stay
	// warm. Anything without a label is a leftover either way.
	for _, w := range traffic {
		if w == nil || derefString(w.Label) == "" {
			continue
		}
		if name := derefString(w.RevisionName); name != "" && name != serving {
			keep = append(keep, name)
		}
	}
	return keep
}

// servingRevision names the revision a traffic block hands everything to.
func servingRevision(traffic []*armappcontainers.TrafficWeight) string {
	for _, w := range traffic {
		if w != nil && derefInt32(w.Weight) == weightAll {
			return derefString(w.RevisionName)
		}
	}
	return ""
}

// pointTraffic rewrites a block so the named label carries everything.
func pointTraffic(
	traffic []*armappcontainers.TrafficWeight,
	label string,
) ([]*armappcontainers.TrafficWeight, error) {
	var (
		out   []*armappcontainers.TrafficWeight
		found bool
		names []string
	)
	for _, w := range traffic {
		if w == nil {
			continue
		}
		l := derefString(w.Label)
		if l == "" {
			// An unlabelled entry has no name to point at and is not something
			// this command can hand traffic to. Dropping it is the intent:
			// afterwards exactly one labelled side serves.
			continue
		}
		names = append(names, l)

		weight := weightNone
		if l == label {
			weight = weightAll
			found = true
		}
		next := *w
		next.Weight = to.Ptr(weight)
		// A pin, if it was not one already: "whatever is newest" cannot be a
		// resting state for either side.
		next.LatestRevision = nil
		out = append(out, &next)
	}

	if !found {
		sort.Strings(names)
		return nil, fmt.Errorf("no traffic label named %q (found %s)",
			label, orNone(strings.Join(names, ", ")))
	}
	return out, nil
}

// patchTemplate writes the template and returns once ARM has accepted it.
//
// It does not wait for readiness the way the direct path does, because waiting
// belongs to a named revision here: staging has to know exactly which revision
// it is about to hand a label, and the app's own latest-ready pointer cannot
// tell it that. See waitRunning.
func (d *Driver) patchTemplate(
	ctx context.Context,
	name string,
	containers []*armappcontainers.Container,
) error {
	timer := logging.Start("patch container app template", "name", name,
		"containers", len(containers))

	poller, err := d.apps.BeginUpdate(ctx, d.file.Cloud.ResourceGroup, name,
		armappcontainers.ContainerApp{
			Properties: &armappcontainers.ContainerAppProperties{
				Template: &armappcontainers.Template{Containers: containers},
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	timer.Done()
	return nil
}

// patchTraffic writes only the traffic block.
//
// A merge patch for the same reason the template write is one: a read never
// returns secret values, so writing the whole resource back would blank every
// secret Terraform declared. Sending just this leaves the rest of the ingress —
// port, external, sticky sessions — exactly as it is.
func (d *Driver) patchTraffic(
	ctx context.Context,
	name string,
	traffic []*armappcontainers.TrafficWeight,
) error {
	timer := logging.Start("patch container app traffic", "name", name,
		"entries", len(traffic))

	poller, err := d.apps.BeginUpdate(ctx, d.file.Cloud.ResourceGroup, name,
		armappcontainers.ContainerApp{
			Properties: &armappcontainers.ContainerAppProperties{
				Configuration: &armappcontainers.Configuration{
					Ingress: &armappcontainers.Ingress{Traffic: traffic},
				},
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("traffic update: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("traffic update: %w", err)
	}
	timer.Done()
	return nil
}

func (d *Driver) deactivate(ctx context.Context, app, revision string) error {
	timer := logging.Start("deactivate revision", "app", app, "revision", revision)
	if _, err := d.revisions.DeactivateRevision(
		ctx, d.file.Cloud.ResourceGroup, app, revision, nil); err != nil {
		// Already off is the state this was asking for. Reporting it as a
		// failure turned a clean abandon into "could not put the traffic back",
		// which was untrue — the traffic went back, and only a redundant call
		// complained.
		if alreadyInState(err) {
			slog.Debug("revision was already deactivated", "app", app, "revision", revision)
			return nil
		}
		return fmt.Errorf("deactivating %s: %w", revision, err)
	}
	timer.Done()
	return nil
}

// activateIfStopped turns a revision back on, for the case where Container Apps
// handed back one an earlier release had switched off.
func (d *Driver) activateIfStopped(ctx context.Context, app, revision string) error {
	got, err := d.revisions.GetRevision(ctx, d.file.Cloud.ResourceGroup, app, revision, nil)
	if err != nil {
		return fmt.Errorf("reading revision %s: %w", revision, err)
	}
	if got.Properties != nil && derefBool(got.Properties.Active) {
		return nil
	}

	slog.Debug("reactivating a revision an earlier release switched off",
		"app", app, "revision", revision)
	timer := logging.Start("activate revision", "app", app, "revision", revision)
	if _, err := d.revisions.ActivateRevision(
		ctx, d.file.Cloud.ResourceGroup, app, revision, nil); err != nil {
		if !alreadyInState(err) {
			return fmt.Errorf("activating %s: %w", revision, err)
		}
	}
	timer.Done()
	return nil
}

// waitRunning waits for one named revision to be able to serve.
//
// Deliberately not the app's latest-ready pointer, which is what this used to
// look at: that pointer is sticky and keeps naming a revision that has since
// been deactivated, so it answers "yes" for something with no replicas. Asking
// the revision itself cannot be fooled that way.
func (d *Driver) waitRunning(
	ctx context.Context,
	app, revision string,
	timeout time.Duration,
) error {
	started := time.Now()
	deadline := started.Add(timeout)
	var (
		strikes    int
		lastReason string
	)

	for {
		got, err := d.revisions.GetRevision(ctx, d.file.Cloud.ResourceGroup, app, revision, nil)
		if err != nil {
			// A read that fails is never a verdict: the revision was created
			// moments ago and ARM may still be catching up.
			slog.Debug("could not read the revision", "app", app,
				"revision", revision, "err", err)
		} else if props := got.Properties; props != nil {
			if !derefBool(props.Active) {
				return fmt.Errorf("%s: revision %s is deactivated", app, revision)
			}

			if props.RunningState != nil &&
				*props.RunningState == armappcontainers.RevisionRunningStateRunning &&
				(props.HealthState == nil ||
					*props.HealthState != armappcontainers.RevisionHealthStateUnhealthy) {
				slog.Debug("revision is running", "app", app, "revision", revision,
					"elapsed", time.Since(started).Round(time.Second))
				return nil
			}

			reason, certain := d.inspectRevision(ctx, app, revision)
			switch {
			case reason == "":
				strikes = 0
			case certain:
				return fmt.Errorf("%s: revision %s %s", app, revision, reason)
			default:
				strikes++
				lastReason = reason
				if strikes >= unhealthyStrikes {
					return fmt.Errorf("%s: revision %s %s", app, revision, reason)
				}
			}
		}

		if time.Now().After(deadline) {
			msg := fmt.Sprintf("%s: revision %s was not running within %s",
				app, revision, timeout)
			if lastReason != "" {
				msg += "; it last reported that it " + lastReason
			}
			return errors.New(msg)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// alreadyInState reports whether ARM refused a change because the thing is
// already in the state that was asked for.
func alreadyInState(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.ErrorCode == "RevisionAlreadyInRequestedState"
}

// deactivateRest switches off every active revision that is not one of the two
// the invariant keeps.
func (d *Driver) deactivateRest(ctx context.Context, app string, keep []string) error {
	pager := d.revisions.NewListRevisionsPager(d.file.Cloud.ResourceGroup, app, nil)

	var problems []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("listing revisions of %s: %w", app, err)
		}
		for _, r := range page.Value {
			if r == nil || r.Properties == nil || !derefBool(r.Properties.Active) {
				continue
			}
			name := derefString(r.Name)
			if name == "" || slices.Contains(keep, name) {
				continue
			}
			if err := d.deactivate(ctx, app, name); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// Traffic reads the split as it is. Errors reading a revision are swallowed:
// this is a diagnostic, and it is most useful in exactly the states where
// something is already wrong.
func (d *Driver) Traffic(ctx context.Context, t *config.Target) ([]target.TrafficEntry, error) {
	app, err := d.getApp(ctx, t.Name)
	if err != nil {
		return nil, err
	}
	cfg := app.Properties.Configuration
	if cfg == nil || cfg.Ingress == nil {
		return nil, fmt.Errorf("container app %s has no ingress", t.Name)
	}

	var out []target.TrafficEntry
	for _, w := range cfg.Ingress.Traffic {
		if w == nil {
			continue
		}
		e := target.TrafficEntry{
			Label:    derefString(w.Label),
			Revision: derefString(w.RevisionName),
			Weight:   int(derefInt32(w.Weight)),
			Latest:   derefBool(w.LatestRevision),
		}
		if e.Revision != "" {
			if tmpl, err := d.revisionTemplate(ctx, t.Name, e.Revision); err == nil {
				name, err := target.PickContainer(
					containerNames(tmpl.Containers), t.Container, appContainer)
				if err == nil {
					if c := findContainer(tmpl.Containers, name); c != nil {
						e.Version = image.Tag(derefString(c.Image))
					}
				}
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}
