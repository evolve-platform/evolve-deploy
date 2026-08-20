// Package gitlog reads the versions a deploy could be moved to.
//
// A version in a deploy config is a git sha, so the repository already holds the
// list of everything that could be deployed and, more usefully, what each one
// was. "9f8e7d6" is not a choice anyone can make; "9f8e7d6 — fix: retry on 429"
// is. That subject line is the whole reason this package exists rather than the
// registry being asked on its own.
//
// What git cannot say is whether a sha was ever built. That is the registry's
// answer, and the two are intersected in the update command.
package gitlog

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ShortLen is how many characters of a sha a version is.
//
// It matches what the pipeline tags images with — `${GITHUB_SHA:0:7}` — and
// that agreement is the only thing that makes a git sha and a registry tag
// comparable at all. A repository that tags differently gets an empty
// intersection, which the update command reports rather than working around.
const ShortLen = 7

// Commit is one candidate version.
type Commit struct {
	// Version is the sha abbreviated to ShortLen: what a config file holds and
	// what a registry tag is named.
	Version string
	// Subject is the first line of the commit message, which is what someone
	// picking a version is actually reading.
	Subject string
}

// Recent returns the last n commits of the repository at dir, newest first.
//
// Merges are kept. A squash-merged pull request is an ordinary commit and a
// merge commit is what CI builds on a merge queue, so dropping either would
// hide versions that exist.
func Recent(ctx context.Context, dir string, n int) ([]Commit, error) {
	// -z separates commits with a NUL and 0x1f the two fields inside one, so a
	// subject containing a tab, a newline or anything else cannot be mistaken
	// for a separator.
	cmd := exec.CommandContext(ctx, "git",
		"log", "-z", fmt.Sprintf("-n%d", n), "--format=%H%x1f%s")
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		// git's own message is the useful one: "not a git repository", or the
		// name of a revision that does not exist.
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("git log in %s: %s",
				dir, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("git log in %s: %w", dir, err)
	}

	commits := make([]Commit, 0, n)
	for record := range strings.SplitSeq(string(out), "\x00") {
		sha, subject, ok := strings.Cut(record, "\x1f")
		if !ok || len(sha) < ShortLen {
			// The trailing empty record after the last NUL, and nothing else:
			// git wrote this format itself.
			continue
		}
		commits = append(commits, Commit{
			Version: sha[:ShortLen],
			Subject: strings.TrimSpace(subject),
		})
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("git log in %s returned no commits", dir)
	}
	return commits, nil
}
