package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/plan"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// samplePlan is roughly the shape of a real release: a service with a long
// target name, one with a short one, and one that waits for both.
func samplePlan() *plan.Plan {
	change := func(service, typ, name string) *target.Change {
		return &target.Change{
			Service:     service,
			Target:      &config.Target{Type: config.TargetType(typ), Name: name},
			FromVersion: "caf9749",
			ToVersion:   "2ed8407",
			Reason:      "version caf9749 -> 2ed8407",
		}
	}

	return &plan.Plan{
		File: &config.File{Path: "deploy/tst.yaml"},
		Services: []*plan.ServicePlan{{
			Service: &config.Service{Name: "discover", Version: "2ed8407",
				Before: []string{"echo check"}, After: []string{"echo publish"}},
			Changes: []*target.Change{
				change("discover", "container-app", "evolve-tst-discover"),
				change("discover", "container-app-job", "evolve-tst-discover-categories"),
			},
		}, {
			Service: &config.Service{Name: "site", Version: "2ed8407",
				DependsOn: []string{"discover"}},
			Changes: []*target.Change{change("site", "container-app", "evolve-tst-site")},
		}},
	}
}

func TestEveryLineIsTaggedAndAligned(t *testing.T) {
	// The whole point of the format: a pipeline log is five services printing at
	// once, and it is only readable if every line says who it is from and the
	// text after the tag starts in the same column.
	var out bytes.Buffer
	p := samplePlan()
	RenderPlan(NewLog(&out, p), p)

	lines := nonEmpty(out.String())
	if len(lines) == 0 {
		t.Fatal("nothing was printed")
	}

	want := -1
	for _, line := range lines {
		if !strings.HasPrefix(line, "[") {
			t.Errorf("line is not tagged: %q", line)
			continue
		}
		body := strings.Index(line, "] ")
		if body < 0 {
			t.Fatalf("line has no tag: %q", line)
		}
		// The tag is padded to the widest name, so the text after it starts at
		// the same offset on every line.
		rest := line[body+1:]
		at := body + 1 + len(rest) - len(strings.TrimLeft(rest, " "))
		if want == -1 {
			want = at
		} else if at != want {
			t.Errorf("text starts at %d, want %d:\n%s", at, want, out.String())
		}
	}
}

func TestTargetsAndDetailsFormColumns(t *testing.T) {
	var out bytes.Buffer
	p := samplePlan()
	RenderPlan(NewLog(&out, p), p)

	var at []int
	for _, line := range nonEmpty(out.String()) {
		if i := strings.Index(line, "caf9749 -> 2ed8407"); i >= 0 {
			at = append(at, i)
		}
	}
	if len(at) != 3 {
		t.Fatalf("found %d version lines, want 3:\n%s", len(at), out.String())
	}
	for _, i := range at[1:] {
		if i != at[0] {
			t.Errorf("versions do not line up:\n%s", out.String())
		}
	}
}

func TestTheReasonIsLeftOutWhenItRepeatsTheVersions(t *testing.T) {
	var out bytes.Buffer
	p := samplePlan()
	RenderPlan(NewLog(&out, p), p)

	if got := strings.Count(out.String(), "version caf9749 -> 2ed8407"); got != 0 {
		t.Errorf("the reason was printed %d time(s) although it says nothing new:\n%s",
			got, out.String())
	}
}

func TestPlanShowsDependenciesAndHooks(t *testing.T) {
	var out bytes.Buffer
	p := samplePlan()
	RenderPlan(NewLog(&out, p), p)

	for _, want := range []string{"waits for", "discover", "hooks", "1 before, 1 after"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not mention %q:\n%s", want, out.String())
		}
	}
}

func nonEmpty(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
