package hooks

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

func TestRunSubstitutesAndCaptures(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{Out: &out, Verbose: true}

	err := r.Run(context.Background(), "purchase", "after",
		[]string{"echo published {{.name}} at {{.version}} to {{.env}}"},
		Vars{"name": "purchase", "version": "abc1234", "env": "tst"})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "published purchase at abc1234 to tst") {
		t.Errorf("output was %q", got)
	}
}

func TestRunStopsAtTheFirstFailure(t *testing.T) {
	// before is a gate: if the first command fails, the rest of the service
	// must not proceed.
	var out bytes.Buffer
	r := &Runner{Out: &out}

	err := r.Run(context.Background(), "purchase", "before",
		[]string{"exit 1", "echo should-not-run"},
		Vars{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(out.String(), "should-not-run") {
		t.Error("a later hook ran after an earlier one failed")
	}
}

func TestUnknownVariableIsAnError(t *testing.T) {
	// Rather than expanding to nothing, which would make a broken hook look
	// like it worked.
	var out bytes.Buffer
	r := &Runner{Out: &out}

	err := r.Run(context.Background(), "purchase", "after",
		[]string{"hive publish --commit {{.nope}}"}, Vars{"version": "abc"})
	if err == nil {
		t.Fatal("expected an error for an unknown variable")
	}
}

func TestDryRunPrintsWithoutRunning(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{Out: &out, DryRun: true}

	if err := r.Run(context.Background(), "purchase", "before",
		[]string{"exit 1"}, Vars{}); err != nil {
		t.Fatalf("dry run should not execute anything: %v", err)
	}
	if !strings.Contains(out.String(), "before: exit 1") {
		t.Errorf("output was %q", out.String())
	}
}

func TestOutputIsTaggedWithTheService(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{Out: &out, Verbose: true}

	if err := r.Run(context.Background(), "discover", "before",
		[]string{"echo checking schema"}, Vars{}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "[discover] checking schema\n" {
		t.Errorf("output was %q", got)
	}
}

func TestTagsAreAlignedToTheWidestName(t *testing.T) {
	// Three package managers printing at once is only readable if the tags
	// form a column.
	var out bytes.Buffer
	r := &Runner{Out: &out, Verbose: true, Width: len("discover") + 2}

	for _, name := range []string{"discover", "site"} {
		if err := r.Run(context.Background(), name, "before",
			[]string{"echo x"}, Vars{}); err != nil {
			t.Fatal(err)
		}
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	if a, b := strings.Index(lines[0], "x"), strings.Index(lines[1], "x"); a != b {
		t.Errorf("output does not line up:\n%s", out.String())
	}
}

func TestATrailingPartialLineIsNotSwallowed(t *testing.T) {
	// A hook that ends without a newline is exactly where a last error tends
	// to sit, so it must still come out.
	var out bytes.Buffer
	r := &Runner{Out: &out, Verbose: true}

	if err := r.Run(context.Background(), "site", "after",
		[]string{"printf 'no trailing newline'"}, Vars{}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "[site] no trailing newline\n" {
		t.Errorf("output was %q", got)
	}
}

func TestConcurrentHooksDoNotShredEachOther(t *testing.T) {
	// The real case: every service's before hook runs at once, each one a
	// package manager with plenty to say. No line may end up half one service
	// and half another.
	var out bytes.Buffer
	r := &Runner{Out: &out, Verbose: true, Width: len("discover") + 2}

	var wg sync.WaitGroup
	for _, name := range []string{"discover", "purchase", "site"} {
		wg.Go(func() {
			err := r.Run(context.Background(), name, "before",
				[]string{"for i in $(seq 1 50); do echo " + name + "-$i; done"}, Vars{})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 150 {
		t.Fatalf("got %d lines, want 150", len(lines))
	}
	for _, line := range lines {
		tag, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			t.Fatalf("line has no body: %q", line)
		}
		body := strings.TrimSpace(rest)
		// The tag and what follows it have to name the same service, or a
		// write from one hook landed inside another's line.
		name := strings.Trim(tag, "[]")
		if !strings.HasPrefix(body, name+"-") {
			t.Errorf("line is tagged %s but reads %q", tag, body)
		}
	}
}

func TestASucceedingHookIsSilentButAFailingOneIsNot(t *testing.T) {
	// A release runs several CLIs per service and none of their output is the
	// answer to what was deployed. But the moment one fails, what it printed is
	// the only account of why — so silence is conditional on success, not on the
	// flag.
	var quiet bytes.Buffer
	r := &Runner{Out: &quiet}
	if err := r.Run(context.Background(), "purchase", "before",
		[]string{"echo checking schema"}, nil); err != nil {
		t.Fatal(err)
	}
	if quiet.String() != "" {
		t.Errorf("a hook that succeeded printed %q", quiet.String())
	}

	var loud bytes.Buffer
	r = &Runner{Out: &loud}
	err := r.Run(context.Background(), "purchase", "before",
		[]string{"echo breaking change detected; exit 1"}, nil)
	if err == nil {
		t.Fatal("a hook that exited non-zero was reported as success")
	}
	if !strings.Contains(loud.String(), "breaking change detected") {
		t.Errorf("the failing hook's output was swallowed: %q", loud.String())
	}
}
