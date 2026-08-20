package console

import (
	"strings"
	"sync"
	"testing"
)

func TestLinesAreTaggedAndPadded(t *testing.T) {
	var out strings.Builder
	l := New(&out, len("[discover]"), 20)

	l.Line("discover", "ecs/discover", "v1 -> v2")
	l.Line("site", "ecs/site", "v1 -> v2")

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	// Both columns line up, which is the only reason this type exists.
	if a, b := strings.Index(lines[0], "ecs/"), strings.Index(lines[1], "ecs/"); a != b {
		t.Errorf("tags do not line up:\n%s", out.String())
	}
	if a, b := strings.Index(lines[0], "v1 ->"), strings.Index(lines[1], "v1 ->"); a != b {
		t.Errorf("details do not line up:\n%s", out.String())
	}
}

func TestNoteSkipsTheLabelColumn(t *testing.T) {
	var out strings.Builder
	l := New(&out, len("[discover]"), 20)
	l.Note("site", "skipped, discover did not deploy")

	if got := out.String(); got != "[site]     skipped, discover did not deploy\n" {
		t.Errorf("output was %q", got)
	}
}

func TestAContinuationLineKeepsItsPlace(t *testing.T) {
	// An empty label still takes its column, so a reason or an environment
	// change sits under the versions it belongs to.
	var out strings.Builder
	l := New(&out, len("[site]"), 12)
	l.Line("site", "ecs/site", "v1 -> v2")
	l.Line("site", "", "+ NEW_VAR")

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if a, b := strings.Index(lines[0], "v1"), strings.Index(lines[1], "+"); a != b {
		t.Errorf("the continuation line is not under the detail column:\n%s", out.String())
	}
}

func TestWriterTagsEveryLineAndKeepsTheLast(t *testing.T) {
	// A subprocess that ends without a newline is exactly where a last error
	// tends to sit, so Close has to let it out.
	var out strings.Builder
	l := New(&out, 0, 0)

	w := l.Writer("discover")
	_, _ = w.Write([]byte("first\nsec"))
	_, _ = w.Write([]byte("ond\nno newline"))
	_ = w.Close()

	want := "[discover] first\n[discover] second\n[discover] no newline\n"
	if got := out.String(); got != want {
		t.Errorf("output was %q, want %q", got, want)
	}
}

func TestAPercentInSubprocessOutputIsNotAFormat(t *testing.T) {
	var out strings.Builder
	l := New(&out, 0, 0)

	w := l.Writer("site")
	_, _ = w.Write([]byte("100% of 3 checks passed\n"))
	_ = w.Close()

	if got := out.String(); got != "[site] 100% of 3 checks passed\n" {
		t.Errorf("output was %q", got)
	}
}

func TestConcurrentWritersDoNotShredEachOther(t *testing.T) {
	// The real case: every service's hooks run at once, each one a package
	// manager with plenty to say. No line may end up half one service and half
	// another.
	var out strings.Builder
	l := New(&out, len("[discover]"), 20)

	var wg sync.WaitGroup
	for _, name := range []string{"discover", "purchase", "site"} {
		wg.Go(func() {
			w := l.Writer(name)
			for i := range 100 {
				_, _ = w.Write([]byte(name))
				_, _ = w.Write([]byte("-"))
				_, _ = w.Write([]byte(string(rune('a' + i%26))))
				_, _ = w.Write([]byte("\n"))
			}
			_ = w.Close()
		})
		wg.Go(func() {
			for range 100 {
				l.Line(name, "ecs/"+name, "still waiting on health, 10s")
			}
		})
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 600 {
		t.Fatalf("got %d lines, want 600", len(lines))
	}
	for _, line := range lines {
		tag, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			t.Fatalf("line has no body: %q", line)
		}
		// The tag and what follows it have to name the same service, or a write
		// from one goroutine landed inside another's line.
		name := strings.Trim(tag, "[]")
		if !strings.Contains(rest, name) {
			t.Errorf("line is tagged %s but reads %q", tag, strings.TrimSpace(rest))
		}
	}
}
