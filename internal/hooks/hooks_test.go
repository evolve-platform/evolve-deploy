package hooks

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunSubstitutesAndCaptures(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{Out: &out}

	err := r.Run(context.Background(), "after",
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

	err := r.Run(context.Background(), "before",
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

	err := r.Run(context.Background(), "after",
		[]string{"hive publish --commit {{.nope}}"}, Vars{"version": "abc"})
	if err == nil {
		t.Fatal("expected an error for an unknown variable")
	}
}

func TestDryRunPrintsWithoutRunning(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{Out: &out, DryRun: true}

	if err := r.Run(context.Background(), "before",
		[]string{"exit 1"}, Vars{}); err != nil {
		t.Fatalf("dry run should not execute anything: %v", err)
	}
	if !strings.Contains(out.String(), "before: exit 1") {
		t.Errorf("output was %q", out.String())
	}
}
