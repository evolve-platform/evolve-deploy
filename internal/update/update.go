// Package update prepares the questions behind `evolve-deploy update` and
// writes the answers back into the config file.
//
// Promoting a release is a copy of the previous environment's file with new
// versions in it, and typing those in by hand is the part that goes wrong: a
// sha nobody can read, from a commit nobody remembers, for a service whose
// image may never have been built. So the choice is made from a list instead —
// every candidate carries the commit subject it came from, and a candidate only
// appears when the artifact for it exists.
//
// The two halves of that come from different places. Git knows what a version
// means and in which order they happened; only the registry knows which ones
// were built. Neither is enough on its own: git alone offers versions that
// cannot be deployed, and a tag list alone is a wall of shas.
package update

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/gitlog"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// queryConcurrency bounds how many services are asked about at once. Same
// reasoning as planning: high enough that the wait is not the sum of every
// service, low enough not to arrive at a registry as a burst.
const queryConcurrency = 8

// Option is one version a service could be moved to.
type Option struct {
	Version string
	// Subject is the commit's first line. It is what makes a version legible,
	// and it is empty only for a version that is no longer in the window of
	// commits that was read.
	Subject string
}

// Choice is one question: what a service runs now and what it could run.
type Choice struct {
	Service string
	Current string
	Options []Option

	// Note says why the list looks the way it does, when that is not obvious:
	// an image the tool cannot list, or nothing built at all. It is shown with
	// the question, because a list with two entries invites the wrong
	// conclusion when the reason is "I could only check one of five targets".
	Note string

	// Line is where this service's version sits in the file, so the questions
	// come in the order someone reading the file afterwards will see them.
	Line int
}

// Questions works out what to ask for each service in f.
//
// Every service is asked about, including one already on the newest version —
// going back to an older release is exactly what someone reaches for during an
// incident, and a tool that only offers to move forward is no use then.
func Questions(
	ctx context.Context,
	f *config.File,
	d target.Driver,
	edit *Editor,
	commits []gitlog.Commit,
) ([]*Choice, error) {
	if err := d.Verify(ctx); err != nil {
		return nil, err
	}

	versions := make([]string, len(commits))
	subjects := make(map[string]string, len(commits))
	for i, c := range commits {
		versions[i] = c.Version
		subjects[c.Version] = c.Subject
	}

	names := f.ServiceNames()
	choices := make([]*Choice, len(names))

	var (
		mu       sync.Mutex
		problems []string
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(queryConcurrency)

	for i, name := range names {
		g.Go(func() error {
			c, err := question(gctx, d, f.Services[name], versions, subjects)
			if err != nil {
				// Collected rather than returned: someone whose credentials are
				// wrong for three services should see three lines, not discover
				// them one run at a time.
				mu.Lock()
				problems = append(problems, err.Error())
				mu.Unlock()
				return nil
			}
			c.Line = edit.Line(name)
			choices[i] = c
			return nil
		})
	}
	_ = g.Wait()

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("could not work out which versions are available:\n  - %s",
			strings.Join(problems, "\n  - "))
	}

	// File order, because that is the order the diff will be read in.
	slices.SortFunc(choices, func(a, b *Choice) int { return a.Line - b.Line })
	return choices, nil
}

// question narrows the candidate versions to the ones every target of one
// service could actually run, which is what makes it a question worth asking.
//
// The intersection is the point: a service is one version across all of its
// targets — a container app and its four jobs share an image, a service and its
// sidecar lambda share a release — so a version is only offered when every one
// of them has an artifact for it. Offering a version that three targets out of
// five have would produce exactly the half-deployed release the plan phase
// exists to prevent.
func question(
	ctx context.Context,
	d target.Driver,
	c *config.Service,
	versions []string,
	subjects map[string]string,
) (*Choice, error) {
	var (
		// nil means "nothing has constrained the list yet", which is not the
		// same as "nothing is available".
		allowed   []string
		unchecked []string
	)

	for _, t := range c.Targets {
		got, err := d.Artifacts(ctx, t, versions)
		switch {
		case errors.Is(err, target.ErrArtifactsUnknown):
			// An image in a registry this tool has no client for is still
			// perfectly deployable, so this target simply does not narrow the
			// list. Saying so beats both a short list and a hard failure.
			unchecked = append(unchecked, t.Label())
			continue
		case err != nil:
			return nil, fmt.Errorf("%s: %s: %w", c.Name, t.Label(), err)
		}

		if allowed == nil {
			allowed = got
			continue
		}
		allowed = target.Present(allowed, setOf(got))
	}
	if allowed == nil {
		allowed = versions
	}

	choice := &Choice{Service: c.Name, Current: c.Version}
	for _, v := range allowed {
		choice.Options = append(choice.Options, Option{Version: v, Subject: subjects[v]})
	}

	switch {
	case len(unchecked) == len(c.Targets):
		choice.Note = fmt.Sprintf(
			"could not check whether these were built (%s) — every commit is listed",
			strings.Join(unchecked, ", "))
	case len(unchecked) > 0:
		choice.Note = "not checked for " + strings.Join(unchecked, ", ")
	case len(choice.Options) == 0:
		choice.Note = "nothing in the commits that were read was ever built for this service"
	}
	return choice, nil
}

func setOf(versions []string) map[string]bool {
	out := make(map[string]bool, len(versions))
	for _, v := range versions {
		out[v] = true
	}
	return out
}
