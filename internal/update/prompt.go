package update

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
)

// ErrAborted is returned when the questions were left unanswered. Nothing has
// been written at that point — the file is only touched once every service has
// an answer.
var ErrAborted = errors.New("aborted")

// keep is the value that means "leave this service alone". The empty string
// cannot collide with a version, and it is what an unanswered question would
// hold anyway, so the safe answer is also the default.
const keep = ""

// listHeight is how many lines one question may take, title and description
// included. Enough to see a handful of releases at once, short enough that a
// question and its answer stay on one screen — beyond it the list scrolls.
const listHeight = 12

// visibleOptions is how many entries fit in listHeight once the title, the
// description and a note have had their lines. A shorter list is left to size
// itself: a fixed height pads the field out to twelve rows, and eight blank
// lines under three versions reads as something having gone missing.
const visibleOptions = 9

// subjectWidth is where a commit subject is cut. A wrapped option is much
// harder to scan than a truncated one, and the first sixty characters of a
// commit message are the part that says what it was.
const subjectWidth = 60

// Interactive reports whether there is someone to ask.
//
// `update` is the one command in this tool that cannot run in a pipeline, and
// failing on that up front is much clearer than a form rendering into a log
// file that nobody is reading.
func Interactive() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Ask puts one question per service and returns the versions that were picked,
// leaving out every service that was left as it was.
//
// A service with nothing to offer is not asked about: a question with one
// answer is not a question. Its Note says why, and the caller prints it.
func Ask(choices []*Choice) (map[string]string, error) {
	return ask(choices, func(f *huh.Form) error { return f.Run() })
}

// ask is Ask with the running of the form handed in, so a test can drive it
// with keystrokes instead of a terminal.
func ask(choices []*Choice, run func(*huh.Form) error) (map[string]string, error) {
	var (
		groups []*huh.Group
		asked  []*Choice
		// One answer per question, addressed by index — huh writes into these
		// while the form runs.
		answers []string
	)

	for _, c := range choices {
		if len(c.Options) == 0 {
			continue
		}
		asked = append(asked, c)
		answers = append(answers, keep)
	}

	for i, c := range asked {
		// "leave as is" first, then the versions newest first. The cursor opens
		// on it, which is both the safe answer to press enter on and the only
		// way the list renders from the top: the field scrolls its viewport to
		// whatever is selected, so preselecting the running version would hide
		// every newer one above it.
		options := []huh.Option[string]{
			huh.NewOption("leave as is — "+c.Current, keep),
		}
		for _, o := range c.Options {
			options = append(options, huh.NewOption(label(o, c.Current), o.Version))
		}

		field := huh.NewSelect[string]().
			Title(c.Service).
			Description(describe(c)).
			Options(options...).
			Value(&answers[i])
		if len(options) > visibleOptions {
			field = field.Height(listHeight)
		}
		groups = append(groups, huh.NewGroup(field))
	}

	if len(groups) == 0 {
		return nil, nil
	}

	if err := run(huh.NewForm(groups...)); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, ErrAborted
		}
		return nil, err
	}

	picked := map[string]string{}
	for i, c := range asked {
		if answers[i] != keep && answers[i] != c.Current {
			picked[c.Service] = answers[i]
		}
	}
	return picked, nil
}

// describe is the line under the service name: what it runs now, and why the
// list looks the way it does when that needs saying.
func describe(c *Choice) string {
	out := "runs " + c.Current
	if subject := subjectOf(c, c.Current); subject != "" {
		out += " — " + subject
	}
	if c.Note != "" {
		out += "\n" + c.Note
	}
	return out
}

// label is one line of the list: the version, then what that version was.
func label(o Option, current string) string {
	out := fmt.Sprintf("%-8s %s", o.Version, truncate(o.Subject, subjectWidth))
	if o.Version == current {
		out += "  (current)"
	}
	return strings.TrimRight(out, " ")
}

func subjectOf(c *Choice, version string) string {
	for _, o := range c.Options {
		if o.Version == version {
			return truncate(o.Subject, subjectWidth)
		}
	}
	return ""
}

func truncate(s string, width int) string {
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "…"
}
