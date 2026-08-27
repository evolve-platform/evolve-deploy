package gcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// Prunable: a Cloud Run service accumulates revisions and nothing there expires.
//
// A job does not. Its history is executions, which Cloud Run does age out on its
// own, and the job itself is one mutable resource rather than a stack of them —
// so there is nothing here to sweep and offering to would be a lie about what
// the command reaches.
func (d *Driver) Prunable(t config.TargetType) bool { return t == config.TypeCloudRun }

// Prune removes the revisions of one service that nothing needs any more.
//
// Read the protected set off the platform, not off a release. A prune is most
// likely to be run long after the last deploy — by a schedule, by someone
// tidying up — and what a release believed about which side was serving is
// exactly the thing that will have moved on since.
func (d *Driver) Prune(
	ctx context.Context,
	t *config.Target,
	dryRun bool,
) ([]target.Pruned, error) {
	svc, err := d.getService(ctx, t.Name)
	if err != nil {
		return nil, err
	}

	revisions, err := d.listRevisions(ctx, t.Name)
	if err != nil {
		return nil, err
	}

	looked := planPrune(revisions, protectedRevisions(svc), time.Now())
	if dryRun {
		return looked, nil
	}

	for i := range looked {
		if !looked[i].Removed() {
			continue
		}
		if err := d.deleteRevision(ctx, t.Name, looked[i].Revision); err != nil {
			// Reported against the revision rather than returned, because one
			// revision that will not go is not a reason to leave the other eight
			// hundred behind. The caller counts these and says so at the end.
			looked[i].Keep = fmt.Sprintf("could not be removed: %v", err)
		}
	}
	return looked, nil
}

// protectedRevisions is every revision of a service that must survive a prune,
// against the reason it does.
//
// Four sources, and the last two are the ones a hand-written script gets wrong.
// A tagged or weighted entry is the obvious half. A LATEST entry names no
// revision at all, so the revision it resolves to has to be asked for
// separately. And since the switch stopped tagging the side it retires, the
// rollback target is reachable only through the note — which is the whole reason
// this belongs in the tool rather than in a `gcloud` loop.
func protectedRevisions(svc *runpb.Service) map[string]string {
	out := map[string]string{}
	keep := func(revision, why string) {
		if revision == "" {
			return
		}
		// First reason wins. They are ordered so the most concrete one is
		// written first, and "serving traffic" reads better than "is the newest".
		if _, seen := out[revision]; !seen {
			out[revision] = why
		}
	}

	for _, w := range svc.GetTraffic() {
		if w == nil {
			continue
		}
		switch {
		case w.GetPercent() > 0:
			keep(shortRevision(w.GetRevision()), "serving traffic")
		case w.GetTag() != "":
			keep(shortRevision(w.GetRevision()), fmt.Sprintf("tagged %s", w.GetTag()))
		default:
			keep(shortRevision(w.GetRevision()), "named in the traffic block")
		}
	}
	if label, revision, ok := parseRollback(svc.GetAnnotations()); ok {
		keep(revision, fmt.Sprintf("recorded as the %s side to roll back to", label))
	}

	// Whatever the traffic block resolves to when it says "the newest", plus the
	// newest itself: a revision created moments ago that has not gone live yet
	// is not rubbish, it is a release in progress.
	keep(shortRevision(svc.GetLatestReadyRevision()), "the newest ready revision")
	keep(shortRevision(svc.GetLatestCreatedRevision()), "the newest revision")

	return out
}

// prunableRevision is the little of a revision this decision needs, so the
// decision itself can be tested without a Cloud Run client.
type prunableRevision struct {
	name    string
	created time.Time
}

// planPrune decides what becomes of each revision, newest first.
//
// Every revision comes back, kept ones included. A cleanup that reports only
// what it destroyed cannot be read before it is run, and this one is destroying
// something that cannot be recovered.
func planPrune(
	revisions []prunableRevision,
	protected map[string]string,
	now time.Time,
) []target.Pruned {
	out := make([]target.Pruned, 0, len(revisions))
	for _, r := range revisions {
		p := target.Pruned{Revision: r.name}

		// A revision with no creation time is one this cannot reason about, and
		// the safe reading of "I do not know how old it is" is to keep it.
		if r.created.IsZero() {
			p.Keep = "has no creation time to judge"
			out = append(out, p)
			continue
		}
		p.Age = now.Sub(r.created)

		switch {
		case protected[r.name] != "":
			p.Keep = protected[r.name]
		case p.Age < target.PruneRetention:
			p.Keep = fmt.Sprintf("kept for %d more day(s)",
				int((target.PruneRetention-p.Age).Hours()/24)+1)
		}
		out = append(out, p)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Age < out[j].Age })
	return out
}

// listRevisions reads every revision of a service.
func (d *Driver) listRevisions(
	ctx context.Context,
	service string,
) ([]prunableRevision, error) {
	timer := logging.Start("list cloud run revisions", "service", service)

	var out []prunableRevision
	it := d.revisions.ListRevisions(ctx, &runpb.ListRevisionsRequest{
		Parent: d.serviceName(service),
	})
	for {
		rev, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing revisions of %s: %w", service, err)
		}
		r := prunableRevision{name: shortRevision(rev.GetName())}
		if ts := rev.GetCreateTime(); ts != nil {
			r.created = ts.AsTime()
		}
		out = append(out, r)
	}

	timer.Done("revisions", len(out))
	return out, nil
}

// deleteRevision removes one revision and waits for it.
//
// Waiting costs the sweep its speed — the first run against a service with eight
// hundred revisions is a long one — and it buys the only thing worth having
// here, which is knowing whether the revision actually went. Cloud Run refuses
// to delete a revision that is serving traffic, so this is also the second lock
// on the door: the protected set decides, and the platform disagrees out loud if
// the tool ever gets it wrong.
func (d *Driver) deleteRevision(ctx context.Context, service, revision string) error {
	timer := logging.Start("delete cloud run revision", "revision", revision)

	op, err := d.revisions.DeleteRevision(ctx, &runpb.DeleteRevisionRequest{
		Name: d.serviceName(service) + "/revisions/" + revision,
	})
	if err != nil {
		return err
	}
	if _, err := op.Wait(ctx); err != nil {
		return err
	}

	timer.Done()
	return nil
}
