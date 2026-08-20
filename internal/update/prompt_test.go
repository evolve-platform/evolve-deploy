package update

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/huh"
)

// keys drives the form: an arrow and a return, the way a terminal sends them.
const (
	down  = "\x1b[B"
	enter = "\r"
)

// run puts the given keystrokes to the form and returns what was picked.
//
// The form is rendered into nothing and read from a string, so this exercises
// the real selects and the real key handling without a terminal.
func run(t *testing.T, choices []*Choice, keys string) map[string]string {
	t.Helper()

	type result struct {
		picked map[string]string
		err    error
	}
	done := make(chan result, 1)

	go func() {
		picked, err := ask(choices, func(f *huh.Form) error {
			return f.
				WithInput(strings.NewReader(keys)).
				WithOutput(io.Discard).
				Run()
		})
		done <- result{picked, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.picked
	case <-time.After(10 * time.Second):
		t.Fatal("the form did not finish — it is probably waiting for a key that was not sent")
		return nil
	}
}

func choice(service, current string, options ...Option) *Choice {
	return &Choice{Service: service, Current: current, Options: options}
}

var (
	newest = Option{Version: "ddd4444", Subject: "chore: bump deps"}
	middle = Option{Version: "ccc3333", Subject: "feat: bulk index endpoint"}
	oldest = Option{Version: "bbb2222", Subject: "fix: retry on 429"}
)

// The list opens on "leave as is", so enter — the reflex answer — has to mean
// "nothing changes".
func TestEnterLeavesTheServiceAlone(t *testing.T) {
	picked := run(t, []*Choice{
		choice("discover", "ccc3333", newest, middle, oldest),
	}, enter)

	if len(picked) != 0 {
		t.Errorf("picked = %v, want nothing", picked)
	}
}

func TestPickingAnOlderVersion(t *testing.T) {
	// The list is "leave as is", then newest to oldest: three down is bbb2222.
	picked := run(t, []*Choice{
		choice("discover", "ccc3333", newest, middle, oldest),
	}, down+down+down+enter)

	if picked["discover"] != "bbb2222" {
		t.Errorf("picked = %v, want discover on bbb2222", picked)
	}
}

// Picking the version a service already runs is not a change, and must not end
// up in the file as one — the summary would report a promotion that did not
// happen.
func TestPickingTheRunningVersionIsNotAChange(t *testing.T) {
	picked := run(t, []*Choice{
		choice("discover", "ccc3333", newest, middle, oldest),
	}, down+down+enter)

	if len(picked) != 0 {
		t.Errorf("picked = %v, want nothing", picked)
	}
}

// Every service is one question, and the answers must not be attributed to the
// wrong one — the whole file is written from this map.
func TestAnswersStayWithTheirService(t *testing.T) {
	// One down is the newest version for the first question; the second is
	// answered with enter, which leaves it alone.
	picked := run(t, []*Choice{
		choice("site", "ccc3333", newest, middle),
		choice("discover", "ccc3333", newest, middle),
	}, down+enter+enter)

	if picked["site"] != "ddd4444" {
		t.Errorf("site = %q, want ddd4444", picked["site"])
	}
	if _, ok := picked["discover"]; ok {
		t.Errorf("discover = %q, want it left alone", picked["discover"])
	}
}

// A service whose current version was never built still has to be askable, and
// the version it runs is then simply not in the list.
func TestCurrentVersionOutsideTheList(t *testing.T) {
	picked := run(t, []*Choice{
		choice("discover", "000abcd", newest, middle),
	}, enter)

	if len(picked) != 0 {
		t.Errorf("picked = %v, want nothing", picked)
	}
}

// Nothing to choose from is not a question. It would otherwise be a prompt with
// one entry that does nothing, once per service that was never built.
func TestServiceWithoutOptionsIsNotAsked(t *testing.T) {
	picked, err := ask([]*Choice{choice("discover", "ccc3333")}, func(*huh.Form) error {
		t.Error("the form should not have run")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(picked) != 0 {
		t.Errorf("picked = %v, want nothing", picked)
	}
}

func TestQuittingIsReportedAsAborted(t *testing.T) {
	_, err := ask([]*Choice{choice("discover", "ccc3333", newest)}, func(*huh.Form) error {
		return huh.ErrUserAborted
	})
	if !errors.Is(err, ErrAborted) {
		t.Errorf("err = %v, want ErrAborted", err)
	}
}
