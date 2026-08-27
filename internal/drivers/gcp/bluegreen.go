package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// The only two weights this tool writes. There is no gradual shift: a canary
// without metrics is a stopwatch, and there are no metrics here.
const (
	weightAll  int32 = 100
	weightNone int32 = 0
)

// bgPayload is what a blue-green change carries from Plan to Apply.
type bgPayload struct {
	// template is the next revision, built from the template of the revision
	// that is *serving* rather than from the service's own — see
	// planServiceBlueGreen.
	template *runpb.RevisionTemplate

	// traffic is the block as it was found, so Abandon can put it back exactly.
	traffic []*runpb.TrafficTarget

	// staged is filled in by Stage and read by Switch and Abandon.
	staged string
	// url is the tagged address of the staged revision, read from the API
	// rather than assembled.
	url string
}

// Routable reports which target types carry traffic. A Cloud Run service does;
// a Cloud Run job has no ingress, so it rides along with the service it shares
// an image with and is written at the switch.
func (d *Driver) Routable(t config.TargetType) bool {
	return t == config.TypeCloudRun
}

// Pointable: the traffic block is the tool's own here too.
func (d *Driver) Pointable(t config.TargetType) bool { return d.Routable(t) }

// Fallback: nothing is switched off here and nothing has to be. A Cloud Run
// revision with no traffic scales itself to zero, and traffic arriving is what
// brings it back — a cold start, handled by the platform.
func (d *Driver) Fallback(*config.Target) string { return "scaled to zero" }

// Sides reads the current split off the service.
func (d *Driver) Sides(ctx context.Context, t *config.Target) (*target.Sides, error) {
	svc, err := d.getService(ctx, t.Name)
	if err != nil {
		return nil, err
	}
	return d.sidesOf(ctx, t, svc)
}

func (d *Driver) sidesOf(
	ctx context.Context,
	t *config.Target,
	svc *runpb.Service,
) (*target.Sides, error) {
	sides, err := readSides(svc.GetTraffic(), t.Strategy.Labels)
	if err != nil {
		return nil, fmt.Errorf("cloud run service %s: %w", t.Name, err)
	}

	// A LATEST target names no revision, so the pin has something to write.
	// Which revision that is, is the service's own answer and not ours to
	// guess.
	if sides.Active.Revision == "" && sides.PinNeeded {
		sides.Active.Revision = shortRevision(svc.GetLatestReadyRevision())
		if sides.Active.Revision == "" {
			return nil, fmt.Errorf(
				"cloud run service %s routes to whatever is newest and has no ready "+
					"revision to pin that to", t.Name)
		}
	}

	// The version that is running is the tag on the revision that is serving,
	// not the tag in the service's template. With two live revisions the
	// template is whichever was created last, which after a failed deploy is
	// the one that was thrown away.
	//
	// It can come back empty, and on Cloud Run more often than not: a revision
	// records the image as the digest the tag resolved to, and keeps no note of
	// what was written — there is no `client.knative.dev/user-image` on a
	// revision this tool can read. So the side that is serving is known, its
	// address is known, and the version on it is not. That is reported as
	// unknown and deployed over, which is the safe end of the trade; the
	// alternative was the digest hex displayed as a release.
	if sides.Active.Revision != "" {
		if rev, err := d.getRevision(ctx, t.Name, sides.Active.Revision); err == nil {
			name, err := target.PickContainer(
				containerNames(rev.GetContainers()), t.Container, cloudRunContainer)
			if err == nil {
				sides.Active.Version = revisionVersion(rev, name)
			}
		} else {
			slog.Debug("could not read the serving revision", "service", t.Name,
				"revision", sides.Active.Revision, "err", err)
		}
	}
	return sides, nil
}

// readSides applies the one rule this whole feature rests on: active is the tag
// carrying 100% of the traffic, and anything else is a refusal.
//
// A split means someone is in here by hand or a previous run died. There is
// then no active side to deploy away from, and carrying on would mean the tool
// deciding for itself which of the two revisions it may throw away.
func readSides(traffic []*runpb.TrafficTarget, labels []string) (*target.Sides, error) {
	if len(labels) != 2 {
		return nil, fmt.Errorf("strategy.labels needs exactly two names, got %d", len(labels))
	}

	var (
		serving []*runpb.TrafficTarget
		byTag   = map[string]*runpb.TrafficTarget{}
	)
	for _, w := range traffic {
		if w == nil {
			continue
		}
		if w.GetPercent() > 0 {
			serving = append(serving, w)
		}
		if tag := w.GetTag(); tag != "" {
			byTag[tag] = w
		}
	}

	// An empty block is Cloud Run's default of everything to the newest, which
	// is a rule rather than a reference and has no tag to fall back to.
	if len(traffic) == 0 {
		return nil, fmt.Errorf(
			"the traffic block is empty, so all of it goes to whatever is newest and "+
				"there is no side to fall back to:\n"+
				"    give one side a tag first with `evolve-deploy traffic <config> --to %s`",
			labels[0])
	}
	if len(serving) != 1 || serving[0].GetPercent() != weightAll {
		return nil, fmt.Errorf(
			"traffic is split, so there is no active side:\n%s\n"+
				"    resolve it first with `evolve-deploy traffic <config> --to %s`",
			describeTraffic(traffic), labels[0])
	}

	active := serving[0]
	tag := active.GetTag()
	if tag == "" {
		// A tag is allowed to ride in an entry of its own. An untagged entry at
		// 100% next to a tagged one aiming at the same revision is one side
		// written in two lines, not two sides — the service's status collapses
		// the pair into a single tagged entry at 100%, and that is the shape a
		// Terraform bootstrap of `traffic { percent = 100 }` plus
		// `traffic { tag = "blue" }` leaves behind. Resolve it rather than
		// refuse the very state the refusal below asks for.
		if alias := aliasOf(active, traffic, labels); alias != nil {
			active, tag = alias, alias.GetTag()
		}
	}
	if tag == "" {
		// The state every Cloud Run service starts in: one implicit LATEST
		// entry, no tag. Nothing here can fix it — `traffic --to` moves a tag
		// that exists and cannot invent the first one — so the refusal names
		// where it comes from instead of a command that would fail differently.
		return nil, fmt.Errorf(
			"100%% of the traffic goes to an entry with no tag, so there is "+
				"nothing to fall back to:\n%s\n"+
				"    A first side has to be bootstrapped in Terraform: give the serving\n"+
				"    revision `tag = \"%s\"` in the service's traffic block, then leave the\n"+
				"    block to the tool with ignore_changes",
			describeTraffic(traffic), labels[0])
	}
	idx := slices.Index(labels, tag)
	if idx < 0 {
		return nil, fmt.Errorf(
			"the tag carrying all the traffic is %q, which is not one of %s:\n%s",
			tag, strings.Join(labels, " or "), describeTraffic(traffic))
	}

	idleLabel := labels[1-idx]
	sides := &target.Sides{
		Active: target.Side{Label: tag, Revision: shortRevision(active.GetRevision())},
		// The idle tag may not exist yet: a service that has only ever been
		// deployed once has one side and no other.
		Idle: target.Side{Label: idleLabel},
		// "Whatever is newest" is a rule, not a reference. While it stands,
		// every new revision takes all the traffic the moment it exists — so it
		// has to become a fact before anything is staged.
		PinNeeded: active.GetType() == runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST,
	}
	if w, ok := byTag[idleLabel]; ok {
		sides.Idle.Revision = shortRevision(w.GetRevision())
	}
	return sides, nil
}

// aliasOf finds the tagged entry that aims at the same revision as the untagged
// entry carrying all the traffic, so the tag can be read as the active side.
//
// Two candidates is not an alias but an ambiguity — both sides claiming the
// serving revision is a state to refuse, not to pick a winner from.
func aliasOf(
	active *runpb.TrafficTarget,
	traffic []*runpb.TrafficTarget,
	labels []string,
) *runpb.TrafficTarget {
	var found *runpb.TrafficTarget
	for _, w := range traffic {
		if w == nil || w == active || !slices.Contains(labels, w.GetTag()) {
			continue
		}
		if !sameRevision(active, w) {
			continue
		}
		if found != nil {
			return nil
		}
		found = w
	}
	return found
}

// sameRevision reports whether two entries resolve to one revision: either both
// follow whatever is newest, or both name the same one. A LATEST entry and a
// pinned one are deliberately not the same even when the pin happens to name
// today's newest revision — one is a rule and the other a reference, and which
// of the two the tag follows is the difference that matters here.
func sameRevision(a, b *runpb.TrafficTarget) bool {
	const latest = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST
	if a.GetType() == latest || b.GetType() == latest {
		return a.GetType() == b.GetType()
	}
	rev := shortRevision(a.GetRevision())
	return rev != "" && rev == shortRevision(b.GetRevision())
}

// describeTraffic renders a traffic block for an error message. Whoever reads a
// refusal needs to see what was actually there.
func describeTraffic(traffic []*runpb.TrafficTarget) string {
	if len(traffic) == 0 {
		return "      (the traffic block is empty)"
	}
	var lines []string
	for _, w := range traffic {
		if w == nil {
			continue
		}
		revision := shortRevision(w.GetRevision())
		if w.GetType() == runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST {
			revision = "(latest)"
		}
		lines = append(lines, fmt.Sprintf("      %-8s %-40s %d%%",
			orNone(w.GetTag()), orNone(revision), w.GetPercent()))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// weights builds a traffic block. A side with no revision is left out: a tag
// cannot point at nothing.
func weights(entries ...trafficEntry) []*runpb.TrafficTarget {
	out := make([]*runpb.TrafficTarget, 0, len(entries))
	for _, e := range entries {
		if e.side.Revision == "" {
			continue
		}
		out = append(out, &runpb.TrafficTarget{
			// Explicitly by revision: the pin is the whole point, and LATEST
			// here would hand the next revision everything the moment it exists.
			Type:     runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION,
			Revision: e.side.Revision,
			Tag:      e.side.Label,
			Percent:  e.weight,
		})
	}
	return out
}

type trafficEntry struct {
	side   target.Side
	weight int32
}

// shortRevision drops the resource path Cloud Run returns in some fields and
// keeps in others. A traffic block is written with the bare name.
func shortRevision(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// getService reads a service and checks the parts every caller needs.
func (d *Driver) getService(ctx context.Context, name string) (*runpb.Service, error) {
	timer := logging.Start("get cloud run service", "name", name)
	svc, err := d.services.GetService(ctx, &runpb.GetServiceRequest{
		Name: d.serviceName(name),
	})
	if err != nil {
		return nil, fmt.Errorf("cloud run service %s: %w", name, err)
	}
	timer.Done()

	if svc.GetTemplate() == nil {
		return nil, fmt.Errorf("cloud run service %s has no template", name)
	}
	return svc, nil
}

// revisionContainers reads one revision's own containers.
//
// This is the read that makes blue-green correct rather than nearly correct.
// With two live revisions the service's template is whichever was created last
// — after a failed deploy, the one that was abandoned. Building on that would
// leak a failed attempt's environment into the next release, and comparing
// against it would report "already up to date" for a version that never
// shipped.
func (d *Driver) revisionContainers(
	ctx context.Context,
	service, revision string,
) ([]*runpb.Container, error) {
	got, err := d.getRevision(ctx, service, revision)
	if err != nil {
		return nil, err
	}
	return got.GetContainers(), nil
}

// getRevision reads one revision whole, for the callers that want the version
// annotation beside the containers and would otherwise read it twice.
func (d *Driver) getRevision(
	ctx context.Context,
	service, revision string,
) (*runpb.Revision, error) {
	got, err := d.revisions.GetRevision(ctx, &runpb.GetRevisionRequest{
		Name: d.serviceName(service) + "/revisions/" + revision,
	})
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision, err)
	}
	if len(got.GetContainers()) == 0 {
		return nil, fmt.Errorf("revision %s has no containers", revision)
	}
	return got, nil
}

// revisionVersion reads the release a revision is running.
//
// The annotation is the answer where there is one, because the image is not:
// Cloud Run records it as a digest, and the hex on its own is not a version. The
// tag is still tried after it, for the revision Terraform created and every
// revision written before this tool started leaving the note — there the image
// may well carry a tag. Nothing found means unknown, which is reported as such
// and deployed over rather than assumed current.
func revisionVersion(rev *runpb.Revision, container string) string {
	if v := rev.GetAnnotations()[versionAnnotation]; v != "" {
		return v
	}
	if c := findContainer(rev.GetContainers(), container); c != nil {
		return image.Tag(c.GetImage())
	}
	return ""
}

// Stage creates the new revision and points the idle tag at it.
//
// The order of the writes is the whole story. While the active target says
// "whatever is newest", every revision created takes all the traffic the moment
// it exists — before the smoke test, before a weight is written. So the pin
// comes first, in a write of its own, and only then is a revision created.
func (d *Driver) Stage(ctx context.Context, ch *target.Change) (*target.Staged, error) {
	p := ch.Payload.(*bgPayload)
	name := ch.Target.Name

	if ch.Sides.PinNeeded {
		slog.Debug("pinning the active tag before staging",
			"service", name, "tag", ch.Sides.Active.Label,
			"revision", ch.Sides.Active.Revision)
		if _, err := d.patchTraffic(ctx, name, weights(
			trafficEntry{ch.Sides.Active, weightAll},
			trafficEntry{ch.Sides.Idle, weightNone},
		)); err != nil {
			return nil, fmt.Errorf("pinning %s before staging: %w", ch.Sides.Active.Label, err)
		}
	}

	// The template write waits for the service to reconcile, which for Cloud
	// Run means the new revision is ready. There is no separate readiness poll
	// the way there is on Container Apps: a container that never starts makes
	// this fail rather than leaving something broken behind.
	svc, err := d.updateTemplate(ctx, name, p.template)
	if err != nil {
		return nil, err
	}
	staged := shortRevision(svc.GetLatestCreatedRevision())
	if staged == "" {
		return nil, fmt.Errorf("cloud run service %s reported no revision after staging", name)
	}
	p.staged = staged

	svc, err = d.patchTraffic(ctx, name, weights(
		trafficEntry{ch.Sides.Active, weightAll},
		trafficEntry{target.Side{Label: ch.Sides.Idle.Label, Revision: staged}, weightNone},
	))
	if err != nil {
		return nil, fmt.Errorf("attaching tag %s to %s: %w", ch.Sides.Idle.Label, staged, err)
	}

	// The tagged URL is returned by the API rather than assembled from the
	// service hostname. Cloud Run gives it out, so there is no reason to guess
	// at it the way Container Apps forces.
	p.url = taggedURL(svc.GetTrafficStatuses(), ch.Sides.Idle.Label)
	if p.url == "" {
		return nil, fmt.Errorf(
			"cloud run service %s reported no address for tag %s, so there is nothing "+
				"to point a smoke test at", name, ch.Sides.Idle.Label)
	}

	return &target.Staged{
		Label:    ch.Sides.Idle.Label,
		Revision: staged,
		URL:      p.url,
	}, nil
}

// Switch hands the staged side all of the traffic, in one write.
func (d *Driver) Switch(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*bgPayload)
	_, err := d.patchTraffic(ctx, ch.Target.Name, weights(
		trafficEntry{target.Side{Label: ch.Sides.Idle.Label, Revision: p.staged}, weightAll},
		trafficEntry{ch.Sides.Active, weightNone},
	))
	return err
}

// Abandon restores the traffic block exactly as it was.
//
// Nothing is deleted. A Cloud Run revision with no traffic scales itself to
// zero and costs nothing, and deleting it would throw away the only copy of a
// template — where Container Apps can deactivate and activate again, this is
// one-way. The revision is left where it is and the next release replaces it.
func (d *Driver) Abandon(ctx context.Context, ch *target.Change) error {
	_, err := d.patchTraffic(ctx, ch.Target.Name, weights(
		trafficEntry{ch.Sides.Active, weightAll},
		trafficEntry{ch.Sides.Idle, weightNone},
	))
	return err
}

// Settle is where the Container Apps driver deactivates the previous revision,
// and here there is nothing to deactivate.
//
// Switch already wrote the canonical two-entry block, which drops any leftover
// entry a run that died halfway left behind — and that is the whole cleanup on
// Cloud Run. A revision that carries no traffic scales itself to zero, so the
// side that stopped serving stops costing anything without being told.
//
// Unless the template carries revision-level minimum instances, which is why
// planServiceBlueGreen refuses keep_warm here: that setting belongs to
// Terraform and this tool cannot honour a promise it does not write.
func (d *Driver) Settle(context.Context, *target.Change) error { return nil }

// Tidy trims the traffic block back to the two tagged entries.
//
// The Container Apps version of this switches revisions off. Here the block
// itself is the only state worth tidying: a stray tag from a run that died
// halfway is a URL that answers, pointing at a version nobody chose.
func (d *Driver) Tidy(ctx context.Context, t *config.Target) error {
	svc, err := d.getService(ctx, t.Name)
	if err != nil {
		return err
	}

	next, dropped := tidyTraffic(svc.GetTraffic(), t.Strategy.Labels)
	if next == nil {
		return fmt.Errorf(
			"cloud run service %s: no single revision carries all the traffic, so there "+
				"is nothing to tidy around:\n%s", t.Name, describeTraffic(svc.GetTraffic()))
	}
	if dropped == 0 {
		return nil
	}
	_, err = d.patchTraffic(ctx, t.Name, next)
	return err
}

// tidyTraffic keeps the entries whose tag is one of the two sides and reports
// how many it dropped. It returns nothing when the block has no single serving
// entry, which is a refusal rather than a guess: dropping the wrong half of a
// split is not a cleanup.
func tidyTraffic(
	traffic []*runpb.TrafficTarget,
	labels []string,
) (next []*runpb.TrafficTarget, dropped int) {
	var serving int
	for _, w := range traffic {
		if w.GetPercent() == weightAll {
			serving++
		}
	}
	if serving != 1 {
		return nil, 0
	}

	for _, w := range traffic {
		if w == nil {
			continue
		}
		if slices.Contains(labels, w.GetTag()) {
			next = append(next, w)
			continue
		}
		dropped++
	}
	return next, dropped
}

// Point puts all the traffic on one tag without staging anything.
//
// It reads the traffic block directly instead of going through Sides, and that
// is deliberate: the state this has to repair is exactly the state Sides
// refuses to interpret.
//
// Nothing has to be started first the way it does on Container Apps. A Cloud
// Run revision at 0% is scaled to zero rather than switched off, and traffic
// arriving is what starts it — a cold one, but the platform handles it.
func (d *Driver) Point(ctx context.Context, t *config.Target, label string) error {
	svc, err := d.getService(ctx, t.Name)
	if err != nil {
		return err
	}

	next, err := pointTraffic(svc.GetTraffic(), label)
	if err != nil {
		return fmt.Errorf("cloud run service %s: %w", t.Name, err)
	}
	_, err = d.patchTraffic(ctx, t.Name, next)
	return err
}

// pointTraffic rewrites a block so the named tag carries everything.
func pointTraffic(
	traffic []*runpb.TrafficTarget,
	label string,
) ([]*runpb.TrafficTarget, error) {
	var (
		out   []*runpb.TrafficTarget
		found bool
		names []string
	)
	for _, w := range traffic {
		if w == nil {
			continue
		}
		tag := w.GetTag()
		if tag == "" {
			// An untagged entry has no name to point at and is not something
			// this command can hand traffic to. Dropping it is the intent:
			// afterwards exactly one tagged side serves.
			continue
		}
		names = append(names, tag)

		weight := weightNone
		if tag == label {
			weight = weightAll
			found = true
		}
		out = append(out, &runpb.TrafficTarget{
			// A pin, if it was not one already: "whatever is newest" cannot be
			// a resting state for either side.
			Type:     runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION,
			Revision: shortRevision(w.GetRevision()),
			Tag:      tag,
			Percent:  weight,
		})
	}

	if !found {
		sort.Strings(names)
		return nil, fmt.Errorf("no traffic tag named %q (found %s)",
			label, orNone(strings.Join(names, ", ")))
	}
	return out, nil
}

// Traffic reads the split as it is. Errors reading a revision are swallowed:
// this is a diagnostic, and it is most useful in exactly the states where
// something is already wrong.
func (d *Driver) Traffic(ctx context.Context, t *config.Target) ([]target.TrafficEntry, error) {
	svc, err := d.getService(ctx, t.Name)
	if err != nil {
		return nil, err
	}

	var out []target.TrafficEntry
	for _, w := range svc.GetTraffic() {
		if w == nil {
			continue
		}
		e := target.TrafficEntry{
			Label:    w.GetTag(),
			Revision: shortRevision(w.GetRevision()),
			Weight:   int(w.GetPercent()),
			Latest: w.GetType() ==
				runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST,
		}
		if e.Revision != "" {
			if rev, err := d.getRevision(ctx, t.Name, e.Revision); err == nil {
				name, err := target.PickContainer(
					containerNames(rev.GetContainers()), t.Container, cloudRunContainer)
				if err == nil {
					e.Version = revisionVersion(rev, name)
				}
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// taggedURL finds the address Cloud Run published for one tag.
func taggedURL(statuses []*runpb.TrafficTargetStatus, tag string) string {
	for _, s := range statuses {
		if s.GetTag() == tag {
			return s.GetUri()
		}
	}
	return ""
}

// updateTemplate writes the template with a mask that leaves traffic alone, so
// staging cannot move a weight by accident.
func (d *Driver) updateTemplate(
	ctx context.Context,
	name string,
	tmpl *runpb.RevisionTemplate,
) (*runpb.Service, error) {
	return d.updateMasked(ctx, "stage cloud run revision", name,
		&runpb.Service{Name: d.serviceName(name), Template: tmpl}, "template")
}

// patchTraffic writes only the traffic block, for the same reason the template
// write is masked the other way: neither half of a release should be able to
// move the other one.
func (d *Driver) patchTraffic(
	ctx context.Context,
	name string,
	traffic []*runpb.TrafficTarget,
) (*runpb.Service, error) {
	return d.updateMasked(ctx, "patch cloud run traffic", name,
		&runpb.Service{Name: d.serviceName(name), Traffic: traffic}, "traffic")
}

// updateMasked is the write both halves share, including the reconcile wait and
// the terminal-condition check that an operation succeeding does not cover.
//
// No etag. On the direct path one is used so two deploys racing cannot silently
// pick a winner; here every write is one of an ordered pair that this process
// made itself moments earlier, and an etag from before the template write would
// fail the traffic write that has to follow it.
func (d *Driver) updateMasked(
	ctx context.Context,
	what, name string,
	svc *runpb.Service,
	mask string,
) (*runpb.Service, error) {
	timer := logging.Start(what, "name", name, "mask", mask)

	op, err := d.services.UpdateService(ctx, &runpb.UpdateServiceRequest{
		Service:    svc,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{mask}},
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", mask, err)
	}
	done, err := op.Wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("waiting for %s: %w", name, err)
	}
	timer.Done("revision", done.GetLatestCreatedRevision(),
		"ready", done.GetLatestReadyRevision())

	if cond := done.GetTerminalCondition(); cond != nil &&
		cond.GetState() != runpb.Condition_CONDITION_SUCCEEDED &&
		cond.GetState() != runpb.Condition_STATE_UNSPECIFIED {
		return nil, fmt.Errorf("%s is %s: %s", name, cond.GetState(), cond.GetMessage())
	}
	return done, nil
}

// planServiceBlueGreen works out the next revision of a Cloud Run service that
// is deployed a side at a time.
//
// It differs from planService in one place that matters more than it looks: the
// template it reads, compares against and builds on is the one belonging to the
// revision that is *serving*, not the service's own. With two live revisions
// the service's template is whichever was created last — which after a failed
// deploy is the revision that was abandoned. Reading that would make the retry
// after a failed smoke test report "already up to date", and would carry the
// failed attempt's environment into the next release.
func (d *Driver) planServiceBlueGreen(
	ctx context.Context,
	want *target.Desired,
) (*target.Change, error) {
	t := want.Target

	// Refused here rather than in config validation, because this is not a
	// contradiction in what was written — it is one cloud being unable to keep
	// a promise the config can legitimately make on another.
	if t.Strategy.KeepsWarm() {
		return nil, fmt.Errorf(
			"cloud run service %s: `keep_warm` cannot be honoured here.\n"+
				"    A revision with no traffic scales itself to zero, and keeping one warm is\n"+
				"    `scaling.min_instance_count` on the template, which Terraform owns — note\n"+
				"    that a service-level minimum does not apply to a tagged revision.\n"+
				"    Remove the setting, or set it on the template and leave it out here",
			t.Name)
	}

	// The same argument for the other half of the pair. `bake_time` is the delay
	// before ECS terminates the old side, and it is ECS that owns that delay —
	// here the previous revision keeps its tag at zero traffic until the next
	// release replaces it, so there is no clock to set. Ignoring the setting
	// would be the worse answer: it is a rollback window someone wrote down, and
	// silently not having one is exactly what they were trying to avoid.
	if t.Strategy.Bake() > 0 {
		return nil, fmt.Errorf(
			"cloud run service %s: `bake_time` cannot be honoured here.\n"+
				"    It is the window before ECS terminates the old side, and Cloud Run never\n"+
				"    terminates one — the previous revision keeps its tag at zero traffic, so\n"+
				"    `rollback` stays available until the next release takes it. Remove the\n"+
				"    setting; the window it buys is already open",
			t.Name)
	}

	svc, err := d.getService(ctx, t.Name)
	if err != nil {
		return nil, err
	}
	sides, err := d.sidesOf(ctx, t, svc)
	if err != nil {
		return nil, err
	}

	// Only fall back to the service's own template when no side is serving yet,
	// which is a service that has never been deployed this way.
	current := svc.GetTemplate()
	if sides.Active.Revision != "" {
		containers, err := d.revisionContainers(ctx, t.Name, sides.Active.Revision)
		if err != nil {
			return nil, fmt.Errorf("cloud run service %s: %w", t.Name, err)
		}
		// Only the containers come from the revision. Everything else on the
		// template — scaling, service account, VPC access — is Terraform's and
		// is carried through from the service, which is where it is current.
		current = proto.Clone(svc.GetTemplate()).(*runpb.RevisionTemplate)
		current.Containers = containers
	}

	name, err := target.PickContainer(
		containerNames(current.GetContainers()), t.Container, cloudRunContainer)
	if err != nil {
		return nil, fmt.Errorf("cloud run service %s: %w", t.Name, err)
	}

	next, from, err := nextTemplate(current, name, want.Version, want.Env, want.ManageEnv)
	if err != nil {
		return nil, err
	}
	// The side goes in last, and after the diff is taken it is invisible — see
	// envFingerprint. It alternates every release, so comparing it would report
	// a change on every run and deploy forever. The variables the config sets
	// per side are invisible for the same reason and go in the same write.
	managed := t.Strategy.SideEnvNames()
	next = withSide(next, name, sides.Idle.Label, want.SideEnv[sides.Idle.Label], managed)

	added, changed, removed := diffEnv(current.GetContainers(), next.GetContainers(), name, managed)

	// Nothing changed here, but the side still needs a revision of its own: the
	// staged side is only a stack if every service is on it, and a revision can
	// hold one tag at a time so the serving one cannot lend it.
	carry := len(added)+len(changed)+len(removed) == 0 && from == want.Version

	return &target.Change{
		Service:     want.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   want.Version,
		Reason:      reason(from, want.Version),
		EnvAdded:    added,
		EnvChanged:  changed,
		EnvRemoved:  removed,
		Sides:       sides,
		Carry:       carry,
		PublicURL:   svc.GetUri(),
		Payload:     &bgPayload{template: next, traffic: svc.GetTraffic()},
	}, nil
}

// withSide writes the side, and the side's own variables, into the application
// container's environment.
//
// It works on whatever environment it is given, which is what makes it correct
// in image-only mode too: there the environment was copied from the serving
// revision untouched, and this writes a few variables over it rather than
// replacing the lot.
//
// `managed` is every variable any side names, and it is dropped before the
// staged side's values go in. That is why it is passed rather than derived from
// `sideEnv`: the containers came from the *other* side's revision, so a variable
// this side does not set would arrive carrying the other side's value. Config
// validation keeps the two sets equal, so in practice this removes exactly what
// it puts back — differently.
func withSide(
	tmpl *runpb.RevisionTemplate,
	name, side string,
	sideEnv []target.EnvVar,
	managed []string,
) *runpb.RevisionTemplate {
	drop := map[string]bool{target.SideEnvVar: true}
	for _, n := range managed {
		drop[n] = true
	}

	out := proto.Clone(tmpl).(*runpb.RevisionTemplate)
	for _, c := range out.GetContainers() {
		if c.GetName() != name {
			continue
		}

		env := make([]*runpb.EnvVar, 0, len(c.GetEnv())+1+len(sideEnv))
		for _, e := range c.GetEnv() {
			if drop[e.GetName()] {
				continue
			}
			env = append(env, e)
		}
		env = append(env, &runpb.EnvVar{
			Name:   target.SideEnvVar,
			Values: &runpb.EnvVar_Value{Value: side},
		})
		c.Env = append(env, renderEnv(sideEnv)...)
	}
	return out
}
