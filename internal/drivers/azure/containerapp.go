package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// readyTimeout bounds the wait for a new revision to become ready. The
// long-running operation finishes as soon as the revision is provisioned, which
// is well before the container has started and passed its probes, so readiness
// is waited for separately.
//
// It is the backstop and not the mechanism. A revision that is never going to
// start is normally recognised as such within a few polls, from the revision
// and the replicas under it; the deadline is for the case where Container Apps
// reports nothing conclusive at all.
const readyTimeout = 10 * time.Minute

// revertTimeout bounds the same wait when putting a template back, and is much
// shorter on purpose.
//
// A revert restores containers that were serving a moment ago, so it either
// comes up quickly or something is wrong that waiting will not fix. Giving it
// the full readyTimeout meant an app where nothing at all can start paid ten
// minutes for the deploy and ten more for the rollback — twenty minutes to
// arrive at a failure that was already certain. The deploy has failed either
// way; the only question left is how long that takes to say.
const revertTimeout = 2 * time.Minute

// pollInterval is how often the app and its newest revision are read while
// waiting. It is the default for Driver.poll, which is what the waits actually
// read: a test of a wait cannot afford five seconds a poll.
const pollInterval = 5 * time.Second

// unhealthyStrikes is how many consecutive polls must call the revision
// unhealthy before the wait gives up on it.
//
// A container that crashes does not report one steady state: Container Apps
// restarts it, so the revision reads Degraded, then Processing, then Degraded
// again. One bad sighting is a container that has not finished starting; three
// in a row is one that is not going to. A revision that is merely slow reports
// Processing throughout and never takes a strike at all.
const unhealthyStrikes = 3

// crashLoopRestarts is the restart count at which a container that is still not
// ready is called doomed rather than slow. Container Apps restarts a container
// that exits immediately, and keeps doing it, so this is reached in well under
// a minute — and a process that has already died three times is not going to
// survive the fourth.
const crashLoopRestarts = 3

type appPayload struct {
	containers []*armappcontainers.Container
	// For rollback: the template exactly as it was before this change.
	previous []*armappcontainers.Container
}

// planApp works out the next revision of a Container App.
//
// Only the application container is touched. The reverse proxy beside it — and
// its proxy_env_vars — is copied through untouched, because that image and its
// settings belong to Terraform.
func (d *Driver) planApp(ctx context.Context, want *target.Desired) (*target.Change, error) {
	if want.Target.Strategy.IsBlueGreen() {
		return d.planAppBlueGreen(ctx, want)
	}

	t := want.Target

	timer := logging.Start("get container app", "name", t.Name)
	got, err := d.apps.Get(ctx, d.file.Cloud.ResourceGroup, t.Name, nil)
	if err != nil {
		return nil, fmt.Errorf("container app %s: %w", t.Name, err)
	}
	timer.Done()
	app := got.ContainerApp
	if app.Properties == nil || app.Properties.Template == nil {
		return nil, fmt.Errorf("container app %s has no template", t.Name)
	}

	current := app.Properties.Template.Containers
	name, err := target.PickContainer(containerNames(current), t.Container, appContainer)
	if err != nil {
		return nil, fmt.Errorf("container app %s: %w", t.Name, err)
	}
	slog.Debug("container chosen", "app", t.Name, "container", name,
		"of", strings.Join(containerNames(current), ","))

	var declared []*armappcontainers.Secret
	if app.Properties.Configuration != nil {
		declared = app.Properties.Configuration.Secrets
	}
	if err := verifySecretRefs(want.Env, declared, "container app "+t.Name); err != nil {
		return nil, err
	}

	next, from, err := nextContainers(
		current, name, want.Version, want.Env, want.ManageEnv, t.Command)
	if err != nil {
		return nil, err
	}

	added, changed, removed := diffContainers(current, next, name, nil)
	command := diffCommand(current, next, name)
	if len(added)+len(changed)+len(removed)+len(command) == 0 && from == want.Version {
		return nil, nil
	}

	return &target.Change{
		Service:     want.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   want.Version,
		Reason: reason(from, want.Version,
			len(added)+len(changed)+len(removed) > 0, len(command) > 0),
		EnvAdded:   added,
		EnvChanged: changed,
		EnvRemoved: removed,
		Command:    command,
		PublicURL:  appURL(appFQDN(&app)),
		Payload:    &appPayload{containers: next, previous: current},
	}, nil
}

func (d *Driver) applyApp(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*appPayload)

	if err := d.patchApp(ctx, ch.Target.Name, p.containers, readyTimeout); err != nil {
		if rbErr := d.patchApp(ctx, ch.Target.Name, p.previous, revertTimeout); rbErr != nil {
			return errors.Join(
				fmt.Errorf("rolling back to %s also failed", target.VersionOrUnknown(ch.FromVersion)),
				err, rbErr)
		}
		return fmt.Errorf("rolled back to %s: %w", target.VersionOrUnknown(ch.FromVersion), err)
	}
	return nil
}

func (d *Driver) revertApp(ctx context.Context, ch *target.Change) error {
	return d.patchApp(ctx, ch.Target.Name, ch.Payload.(*appPayload).previous, revertTimeout)
}

// patchApp sends only the template.
//
// A merge patch rather than a full PUT, and deliberately so: Get never returns
// secret *values*, so writing the whole resource back would blank every secret
// Terraform declared. Sending just the template leaves configuration, ingress
// and secrets exactly as they are.
func (d *Driver) patchApp(
	ctx context.Context,
	name string,
	containers []*armappcontainers.Container,
	timeout time.Duration,
) error {
	timer := logging.Start("patch container app", "name", name,
		"containers", len(containers))

	poller, err := d.apps.BeginUpdate(ctx, d.file.Cloud.ResourceGroup, name, armappcontainers.ContainerApp{
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

	return d.waitReady(ctx, name, timeout)
}

// waitReady waits until the revision that was just created is the one serving.
//
// The long-running operation above completes when the revision is provisioned,
// which says nothing about whether the container started. Container Apps only
// promotes a revision to ready once its probes pass, so comparing the latest
// revision against the latest *ready* revision is the real health check.
//
// That comparison can only ever say "not yet". The app resource carries no
// failure signal: its provisioning state stays Succeeded while the container
// inside it crash loops, and the ready revision simply lags behind. So on the
// app alone this loop has exactly one way to end badly, which is the deadline —
// ten minutes of waiting for something the platform decided in ten seconds. The
// revision underneath does carry that signal, and reading it is what turns the
// wait into a report.
func (d *Driver) waitReady(ctx context.Context, name string, timeout time.Duration) error {
	started := time.Now()
	deadline := started.Add(timeout)
	var (
		lastState  string
		lastReason string
		strikes    int
	)

	slog.Debug("waiting for the revision to become ready",
		"name", name, "timeout", timeout)

	for {
		got, err := d.apps.Get(ctx, d.file.Cloud.ResourceGroup, name, nil)
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", name, err)
		}
		props := got.Properties
		if props == nil {
			return fmt.Errorf("waiting for %s: no properties returned", name)
		}

		if props.ProvisioningState != nil {
			lastState = string(*props.ProvisioningState)
			switch *props.ProvisioningState {
			case armappcontainers.ContainerAppProvisioningStateFailed,
				armappcontainers.ContainerAppProvisioningStateCanceled:
				return fmt.Errorf("%s went to %s", name, lastState)
			}
		}

		latest := derefString(props.LatestRevisionName)
		ready := derefString(props.LatestReadyRevisionName)

		slog.Debug("poll", "name", name, "latest", latest,
			"ready", orNone(ready), "state", lastState,
			"elapsed", time.Since(started).Round(time.Second))

		if latest != "" && latest == ready {
			slog.Debug("revision is ready", "name", name, "revision", latest,
				"elapsed", time.Since(started).Round(time.Second))
			return nil
		}

		if latest != "" {
			reason, certain := d.inspectRevision(ctx, name, latest)
			switch {
			case reason == "":
				// Back to Processing after a bad reading: the container was
				// starting, not failing. The count starts over, so only a run
				// of consecutive failures ends the wait.
				strikes = 0
			case certain:
				return fmt.Errorf("revision %s %s", latest, reason)
			default:
				strikes++
				lastReason = reason
				slog.Debug("revision looks unhealthy", "name", name,
					"revision", latest, "reason", reason,
					"strikes", strikes, "of", unhealthyStrikes)
				if strikes >= unhealthyStrikes {
					return fmt.Errorf("revision %s %s", latest, reason)
				}
			}
		}

		if time.Now().After(deadline) {
			// Nothing was ever conclusive, so whatever the revision last said
			// about itself is the only clue there is. Better in the error than
			// behind a --verbose rerun that will not reproduce it.
			msg := fmt.Sprintf(
				"revision %s did not become ready within %s (ready revision is %s, state %s)",
				latest, timeout, orNone(ready), lastState)
			if lastReason != "" {
				msg += "; the revision last reported that it " + lastReason
			}
			return errors.New(msg)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.poll):
		}
	}
}

// inspectRevision reads the revision the app is trying to promote, and the
// replicas under it, and says whether it can still become ready.
//
// It returns the reason it cannot, and whether that reason is certain. Certain
// means the platform has stopped trying, or a container has already died more
// often than a slow start can explain, and is acted on at once. An uncertain
// reason is only a strike — see unhealthyStrikes.
//
// A read that fails is never a verdict. The revision is created moments before
// this first runs, and a 404 while ARM catches up, or a throttled call, must
// not fail a deploy that is going fine. Those are logged and read as "nothing
// to report", which leaves readyTimeout as the backstop it is meant to be.
func (d *Driver) inspectRevision(ctx context.Context, app, revision string) (reason string, certain bool) {
	got, err := d.revisions.GetRevision(ctx, d.file.Cloud.ResourceGroup, app, revision, nil)
	if err != nil {
		slog.Debug("could not read the revision", "name", app,
			"revision", revision, "err", err)
		return "", false
	}

	reason, certain = classifyRevision(got.Properties)
	if certain {
		return reason, certain
	}

	// The replicas are read even when the revision says nothing is wrong, and
	// that case is what the extra call is for: Container Apps leaves a revision
	// in Processing while the container inside it exits and is restarted, and it
	// will sit there for the whole of readyTimeout. Ten minutes of waiting for a
	// process that died in the first ten seconds, because the one fact that
	// settles it — the restart count — is a level down from where the revision
	// reports. Where the revision has already reported badly the replicas say
	// what it would not: whether "Degraded" is a suspicion or a fact, and which
	// container to go and read.
	detail, container, crashing := d.inspectReplicas(ctx, app, revision)
	reason, certain = weighReplicas(reason, crashing)
	if reason == "" || detail == "" {
		return reason, certain
	}

	// Where there is a reason, the containers are the whole of it: what they
	// report, and where to read what they printed before they gave up.
	return fmt.Sprintf("%s (container %s). Logs: %s",
		reason, detail, d.logsCommand(app, revision, container)), certain
}

// weighReplicas settles what the revision and its replicas say between them.
//
// A crashing container outranks everything the revision reported, including a
// revision that reported nothing: "is Unhealthy" — or Processing — buries the
// one fact that matters, which is that the container is dying on start and the
// revision never ran at all.
//
// With nothing crashing, a revision that says nothing keeps saying nothing. A
// container that is not up and has not died is a container that is starting,
// which is what every deploy looks like while it works; a strike here would end
// a release that is going fine after fifteen seconds.
func weighReplicas(reason string, crashing bool) (string, bool) {
	if crashing {
		return "never started", true
	}
	return reason, false
}

// logsCommand is the command that answers the question a failed revision
// raises. The tool cannot read the container's own output — that is a different
// API and a different permission — but it knows every argument needed to go and
// look, and a message that stops at "it crashed" makes whoever reads it
// reassemble those arguments from three places.
func (d *Driver) logsCommand(app, revision, container string) string {
	cmd := fmt.Sprintf(
		"az containerapp logs show -g %s -n %s --revision %s",
		d.file.Cloud.ResourceGroup, app, revision)
	if container != "" {
		cmd += " --container " + container
	}
	return cmd + " --type console --tail 50"
}

// classifyRevision reads a revision the way the portal does: the platform's own
// provisioning verdict first, then what it says about the containers.
func classifyRevision(props *armappcontainers.RevisionProperties) (reason string, certain bool) {
	if props == nil {
		return "", false
	}

	// Provisioning is the platform's own verdict and it does not change its
	// mind: a failure here is an image that cannot be pulled, a quota, a
	// template it will not accept. ProvisioningError carries the sentence the
	// portal shows, which is usually the whole answer.
	if props.ProvisioningState != nil &&
		*props.ProvisioningState == armappcontainers.RevisionProvisioningStateFailed {
		if e := derefString(props.ProvisioningError); e != "" {
			return "failed to provision: " + e, true
		}
		return "failed to provision", true
	}

	// Running and health state are about the containers, and both report badly
	// while one is still on its way up — so they are suspicion, not verdict. A
	// revision that is merely slow sits in Processing and matches none of this.
	if props.RunningState != nil {
		switch *props.RunningState {
		case armappcontainers.RevisionRunningStateFailed,
			armappcontainers.RevisionRunningStateDegraded:
			return "is " + string(*props.RunningState), false
		}
	}
	if props.HealthState != nil &&
		*props.HealthState == armappcontainers.RevisionHealthStateUnhealthy {
		return "is Unhealthy", false
	}
	return "", false
}

// inspectReplicas describes what the containers in a revision are doing. One
// extra read per poll while a revision is not yet serving, which buys the
// crash loop that the revision's own state does not admit to.
func (d *Driver) inspectReplicas(
	ctx context.Context,
	app, revision string,
) (detail, container string, crashing bool) {
	got, err := d.replicas.ListReplicas(ctx, d.file.Cloud.ResourceGroup, app, revision, nil)
	if err != nil {
		slog.Debug("could not list the replicas", "name", app,
			"revision", revision, "err", err)
		return "", "", false
	}
	return describeReplicas(got.Value)
}

// describeReplicas summarises the containers that are not up, says whether one
// of them is past saving, and names one to point a logs command at.
//
// Restarts are what separate a slow start from a crash loop: a container that
// exits is restarted immediately and indefinitely, so a count in the low single
// digits is already a process that cannot stay up. RunningStateDetails is the
// string the portal shows next to it — an exit code, or the reason the image
// never ran at all.
func describeReplicas(
	replicas []*armappcontainers.Replica,
) (detail, container string, crashing bool) {
	var seen []string
	for _, r := range replicas {
		if r == nil || r.Properties == nil {
			continue
		}
		for _, c := range slices.Concat(r.Properties.InitContainers, r.Properties.Containers) {
			if c == nil || derefBool(c.Ready) {
				// A container that is up says nothing about why the revision is
				// not; the one next to it is the story.
				continue
			}

			restarts := derefInt32(c.RestartCount)
			if restarts >= crashLoopRestarts {
				crashing = true
			}

			name := derefString(c.Name)
			if container == "" || restarts >= crashLoopRestarts {
				// The one to read the logs of is the one that is dying, not
				// whichever container happened to be listed first.
				container = name
			}

			line := name
			// The state and the detail are two ways of saying the same thing —
			// "is Waiting" next to "CrashLoopBackOff" adds a word and no fact —
			// so the detail wins where there is one.
			switch got := cleanDetail(derefString(c.RunningStateDetails)); {
			case got != "":
				line += ": " + got
			case c.RunningState != nil:
				line += " is " + string(*c.RunningState)
			default:
				line += " is not ready"
			}
			if restarts > 0 {
				line += ", " + restartCount(restarts)
			}

			// Every replica of a broken revision reports the same thing, and
			// three copies of one sentence is not three times the information.
			if !slices.Contains(seen, line) {
				seen = append(seen, line)
			}
		}
	}
	sort.Strings(seen)
	return strings.Join(seen, "; "), container, crashing
}

// cleanDetail keeps the fact out of what Container Apps writes for a portal
// blade.
//
// "Container is waiting with reason: CrashLoopBackOff on legion." is one fact, a
// prefix restating the state printed next to it, and the name of the node that
// happened to run it — none of which survives being read at the end of a long
// error line. The reason is the whole of it.
func cleanDetail(s string) string {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "."))

	// Only split on " on " where the prefix matched, because only then is the
	// rest of the sentence known to be "<reason> on <node>".
	if rest, ok := cutPrefixFold(s, "container is waiting with reason:"); ok {
		reason, _, _ := strings.Cut(strings.TrimSpace(rest), " on ")
		return reason
	}
	// "Container exited with code 1" reads as a sentence about the container it
	// is already printed beside. With the space, so that a detail about
	// something merely starting with the word survives whole.
	if rest, ok := cutPrefixFold(s, "container "); ok {
		return strings.TrimSpace(rest)
	}
	return s
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

func restartCount(n int32) string {
	if n == 1 {
		return "restarted once"
	}
	return fmt.Sprintf("restarted %d times", n)
}

// nextContainers copies the template and replaces the image tag of one
// container, its environment when the config declares one, and its entry point
// when the config declares that. It returns the version that was running, read
// back from the image tag.
func nextContainers(
	current []*armappcontainers.Container,
	name, version string,
	env []target.EnvVar,
	manageEnv bool,
	command []string,
) (next []*armappcontainers.Container, from string, err error) {
	next = make([]*armappcontainers.Container, 0, len(current))
	for _, c := range current {
		if c == nil {
			continue
		}
		if derefString(c.Name) != name {
			// A sidecar. Its image and environment are Terraform's.
			next = append(next, c)
			continue
		}

		img, err := image.Retag(derefString(c.Image), version)
		if err != nil {
			return nil, "", err
		}
		from = image.Tag(derefString(c.Image))

		replaced := *c
		replaced.Image = to.Ptr(img)
		replaced.Env = containerEnv(c.Env, renderEnv(env), manageEnv)
		if len(command) > 0 {
			// Args go with it. Container Apps follows Kubernetes, where args are
			// appended to command, so arguments left from what Terraform declared
			// would extend a command line the config meant to replace.
			replaced.Command = to.SliceOfPtrs(command...)
			replaced.Args = nil
		}
		next = append(next, &replaced)
	}

	if findContainer(next, name) == nil {
		return nil, "", fmt.Errorf("container %q disappeared while building the revision", name)
	}
	return next, from, nil
}

// `ignore` is the variables the tool writes per side: their value differs by
// side on purpose, so comparing the staged side's against the serving side's
// would report a change on every run and deploy forever.
func diffContainers(
	current, next []*armappcontainers.Container,
	name string,
	ignore []string,
) (added, changed, removed []string) {
	var have, want map[string]string
	if c := findContainer(current, name); c != nil {
		have = envFingerprint(c.Env, ignore)
	}
	if c := findContainer(next, name); c != nil {
		want = envFingerprint(c.Env, ignore)
	}

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

// diffCommand reports the entry point the next container carries, when it is
// not the one the current container has. Nil means unchanged, which is also
// what a config declaring no command produces: nothing is written, so nothing
// can differ.
func diffCommand(current, next []*armappcontainers.Container, name string) []string {
	have := containerCommand(findContainer(current, name))
	want := containerCommand(findContainer(next, name))
	if slices.Equal(have, want) {
		return nil
	}
	return want
}

func containerCommand(c *armappcontainers.Container) []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Command))
	for _, v := range c.Command {
		out = append(out, derefString(v))
	}
	return out
}

// reason says why a target is being touched, and at an unchanged version it has
// to name which of the two things a deploy owns has moved. Reading "environment
// changed" on a release that rewrote an entry point sends the reader looking for
// a variable that did not change.
func reason(from, to string, envChanged, commandChanged bool) string {
	if from != to {
		return fmt.Sprintf("version %s -> %s", target.VersionOrUnknown(from), to)
	}
	switch {
	case envChanged && commandChanged:
		return "environment and command changed"
	case commandChanged:
		return "command changed"
	default:
		return "environment changed"
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func sortedNames(m map[string]bool) []string {
	return slices.Sorted(maps.Keys(m))
}

func parseJSONMap(where, raw string) (map[string]string, error) {
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%s does not hold a JSON object of strings: %w", where, err)
	}
	return out, nil
}

// planAppBlueGreen works out the next revision of a Container App that is
// deployed a side at a time.
//
// It differs from planApp in reading two templates where planApp reads one: the
// app's own to build on, and the serving revision's to compare against. Which
// one is used where is the whole of this function, and swapping them costs
// either Terraform's ownership of the container or the ability to retry a
// release whose gate failed.
func (d *Driver) planAppBlueGreen(ctx context.Context, want *target.Desired) (*target.Change, error) {
	t := want.Target

	// Refused here rather than in config validation, because this is not a
	// contradiction in what was written — it is one cloud being unable to keep a
	// promise the config can legitimately make on another. `keep_warm` is the
	// setting that answers this question here, and having both spellings do
	// something would make the pair mean two things at once.
	if t.Strategy.Bake() > 0 {
		return nil, fmt.Errorf(
			"container app %s: `bake_time` cannot be honoured here.\n"+
				"    It is the window before ECS terminates the old side, and nothing here\n"+
				"    terminates one on a clock — the previous revision is deactivated at the\n"+
				"    end of the release, or left standing by `keep_warm`. Use `keep_warm` if\n"+
				"    the old side should stay available to roll back to",
			t.Name)
	}

	app, err := d.getApp(ctx, t.Name)
	if err != nil {
		return nil, err
	}
	sides, err := d.sidesOf(ctx, t, app)
	if err != nil {
		return nil, err
	}

	// The revision is built on the app's own template, because Terraform owns
	// everything in the container except the tag. A probe, a cpu bump or an
	// environment variable it declares has to reach the next release; building on
	// the serving revision instead stages over it every release, so the change is
	// reverted rather than delayed and Terraform never owns what it declares.
	//
	// It is compared against the revision that is serving, because with two live
	// revisions the app's template is whichever was created last — after a failed
	// deploy, the attempt that was abandoned. Comparing against that would report
	// the retry as already up to date.
	base := app.Properties.Template.Containers
	serving := base
	if sides.Active.Revision != "" {
		tmpl, err := d.revisionTemplate(ctx, t.Name, sides.Active.Revision)
		if err != nil {
			return nil, fmt.Errorf("container app %s: %w", t.Name, err)
		}
		serving = tmpl.Containers
	}

	name, err := target.PickContainer(containerNames(base), t.Container, appContainer)
	if err != nil {
		return nil, fmt.Errorf("container app %s: %w", t.Name, err)
	}
	slog.Debug("container chosen", "app", t.Name, "container", name,
		"of", strings.Join(containerNames(base), ","))

	var declared []*armappcontainers.Secret
	if app.Properties.Configuration != nil {
		declared = app.Properties.Configuration.Secrets
	}
	if err := verifySecretRefs(want.Env, declared, "container app "+t.Name); err != nil {
		return nil, err
	}

	next, from, err := nextContainers(
		base, name, want.Version, want.Env, want.ManageEnv, t.Command)
	if err != nil {
		return nil, err
	}
	// The tag the base carries is the last one written, which after a failed
	// deploy names a release that never took traffic. What is live is the tag on
	// the serving revision — and an app that has never been deployed has neither,
	// so the base's tag stands in.
	if sides.Active.Version != "" {
		from = sides.Active.Version
	}
	// The side goes in last, and after the diff is taken it is invisible — see
	// envFingerprint. It alternates every release, so comparing it would report
	// a change on every run and deploy forever. The variables the config sets
	// per side are invisible for the same reason and go in the same write.
	sideEnv := want.SideEnv[sides.Idle.Label]
	if err := verifySecretRefs(sideEnv, declared, "container app "+t.Name); err != nil {
		return nil, err
	}
	managed := t.Strategy.SideEnvNames()
	next = withSide(next, name, sides.Idle.Label, sideEnv, managed)

	added, changed, removed := diffContainers(serving, next, name, managed)
	command := diffCommand(serving, next, name)

	// Nothing changed here, but the side still needs a revision of its own: the
	// staged side is only a stack if every app is on it, and a revision can hold
	// one label at a time so the serving one cannot lend it. Whether this is
	// deployed is the release's call, not this target's — see Change.Carry.
	carry := len(added)+len(changed)+len(removed)+len(command) == 0 && from == want.Version

	fqdn := appFQDN(app)

	return &target.Change{
		Service:     want.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   want.Version,
		Reason: reason(from, want.Version,
			len(added)+len(changed)+len(removed) > 0, len(command) > 0),
		EnvAdded:   added,
		EnvChanged: changed,
		EnvRemoved: removed,
		Command:    command,
		Sides:      sides,
		Carry:      carry,
		PublicURL:  appURL(fqdn),
		Payload:    &bgPayload{containers: next, fqdn: fqdn},
	}, nil
}

// containerEnv is the environment the next revision carries for the container
// being released.
//
// The deploy config is the whole declaration, so a name it does not mention is
// gone. Laying the config over what was already there instead could never remove
// anything -- and on Azure nothing else can either, because `env` is on the
// container module's `ignore_changes`: Terraform writes one at create and can
// never correct it. A variable set once therefore outlived every release and went
// on outranking whatever was meant to replace it, which is a silent way to serve
// a value nobody can find the source of. Configuration that used to arrive that
// way belongs in App Configuration or Parameter Store, where the service reads it
// for itself.
//
// A config that declares no environment at all keeps what is there. That is not
// the same ambiguity: it is a repository that has not moved its environment here
// yet, and emptying one would be a poor way to tell it so.
//
// Only the released container is passed here, so a sidecar's environment stays
// Terraform's. The side variable is written afterwards by withSide, so replacing
// here cannot drop it.
func containerEnv(
	current, declared []*armappcontainers.EnvironmentVar,
	manage bool,
) []*armappcontainers.EnvironmentVar {
	if !manage {
		return current
	}
	return declared
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
	containers []*armappcontainers.Container,
	name, side string,
	sideEnv []target.EnvVar,
	managed []string,
) []*armappcontainers.Container {
	drop := map[string]bool{target.SideEnvVar: true}
	for _, n := range managed {
		drop[n] = true
	}

	out := make([]*armappcontainers.Container, 0, len(containers))
	for _, c := range containers {
		if c == nil || derefString(c.Name) != name {
			out = append(out, c)
			continue
		}

		env := make([]*armappcontainers.EnvironmentVar, 0, len(c.Env)+1+len(sideEnv))
		for _, e := range c.Env {
			if e == nil || drop[derefString(e.Name)] {
				continue
			}
			env = append(env, e)
		}

		replaced := *c
		replaced.Env = append(env, &armappcontainers.EnvironmentVar{
			Name:  to.Ptr(target.SideEnvVar),
			Value: to.Ptr(side),
		})
		replaced.Env = append(replaced.Env, renderEnv(sideEnv)...)
		out = append(out, &replaced)
	}
	return out
}
