package gitlog

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// repo builds a throwaway repository with one commit per subject, in the order
// given, and returns its path. The commits come out of Recent in the reverse of
// that order.
func repo(t *testing.T, subjects ...string) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// A developer machine has a global identity and a CI runner does not,
		// so both are pinned here rather than assumed.
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "--quiet")
	for _, subject := range subjects {
		run("commit", "--quiet", "--allow-empty", "-m", subject)
	}
	return dir
}

func TestRecentIsNewestFirst(t *testing.T) {
	dir := repo(t, "first", "second", "third")

	got, err := Recent(context.Background(), dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d commits, want 3", len(got))
	}

	want := []string{"third", "second", "first"}
	for i, subject := range want {
		if got[i].Subject != subject {
			t.Errorf("commit %d subject = %q, want %q", i, got[i].Subject, subject)
		}
		if len(got[i].Version) != ShortLen {
			t.Errorf("commit %d version = %q, want %d characters",
				i, got[i].Version, ShortLen)
		}
	}
}

func TestRecentHonoursTheLimit(t *testing.T) {
	dir := repo(t, "a", "b", "c", "d")

	got, err := Recent(context.Background(), dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d commits, want 2", len(got))
	}
	if got[0].Subject != "d" {
		t.Errorf("first subject = %q, want d", got[0].Subject)
	}
}

// A subject containing the field separator, a tab or a newline must not be
// mistaken for the end of a record — which is why the format is NUL-separated.
func TestRecentKeepsAwkwardSubjectsWhole(t *testing.T) {
	awkward := "fix: a tab\tand a \x1f separator, both inside the subject"
	dir := repo(t, awkward)

	got, err := Recent(context.Background(), dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Subject != awkward {
		t.Errorf("subject = %q, want %q", got[0].Subject, awkward)
	}
}

func TestRecentReportsGitsOwnError(t *testing.T) {
	// A temp dir that is not a repository. The message has to name the reason,
	// because "exit status 128" is not something anyone can act on.
	_, err := Recent(context.Background(), t.TempDir(), 5)
	if err == nil {
		t.Fatal("want an error for a directory that is not a repository")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error = %q, want it to mention that this is not a repository", err)
	}
}
