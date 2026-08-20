package update

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Editor changes the versions in a config file and leaves everything else
// alone.
//
// Not by parsing the file and writing it back out: yaml.Marshal would drop
// every comment, reorder the keys into whatever the structs happen to be, and
// expand the `{ type: ecs, name: purchase }` one-liners into five lines each.
// The result would be a diff nobody can review, for a file whose entire purpose
// is being reviewed.
//
// So the file stays a list of lines and only the scalar holding a version is
// replaced. The parse tree is used for nothing but finding out where that
// scalar is — which is what makes this safe, rather than a regexp that would
// also match the word "version" in a comment or an environment variable.
type Editor struct {
	path  string
	mode  os.FileMode
	lines []string

	// at maps a service name to where its version sits.
	at map[string]token
}

// token is one scalar in the file: which line, which column, and how it was
// written. Style is kept so a quoted version stays quoted — rewriting
// `version: "abc1234"` as `version: def5678` is a change to the file that
// nobody asked for.
type token struct {
	line, column int
	value        string
	style        yaml.Style
}

// Open reads a config file for editing.
func Open(path string) (*Editor, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	e := &Editor{
		path: path,
		mode: info.Mode().Perm(),
		// Splitting on \n and joining on it again keeps the final newline, or
		// its absence, exactly as it was.
		lines: strings.Split(string(raw), "\n"),
		at:    map[string]token{},
	}

	services := child(document(&doc), "services")
	if services == nil {
		return nil, fmt.Errorf("%s: no services", path)
	}
	// A mapping's Content alternates key, value, key, value.
	for i := 0; i+1 < len(services.Content); i += 2 {
		name, body := services.Content[i], services.Content[i+1]
		version := child(body, "version")
		if version == nil {
			continue
		}
		e.at[name.Value] = token{
			line:   version.Line,
			column: version.Column,
			value:  version.Value,
			style:  version.Style,
		}
	}
	return e, nil
}

// Line is where a service's version sits, or zero when the file does not say.
// It orders the questions, so an unknown service sorting first is harmless.
func (e *Editor) Line(service string) int { return e.at[service].line }

// Set replaces one service's version.
//
// It verifies that the file really does hold the old version at the recorded
// position before writing, and refuses otherwise. That guard is what makes
// line-level editing acceptable: on anything unexpected — a multi-line scalar,
// an anchor, a version written in a way the position does not describe — it
// stops instead of corrupting the file.
func (e *Editor) Set(service, version string) error {
	at, ok := e.at[service]
	if !ok {
		return fmt.Errorf("%s: no service named %s", e.path, service)
	}
	if at.line < 1 || at.line > len(e.lines) {
		return fmt.Errorf("%s: version of %s is not on a line this file has", e.path, service)
	}

	line := e.lines[at.line-1]
	start := at.column - 1
	old := quote(at.value, at.style)

	if start < 0 || start+len(old) > len(line) || line[start:start+len(old)] != old {
		return fmt.Errorf(
			"%s:%d: expected the version %s here and found something else — "+
				"edit this file by hand", e.path, at.line, old)
	}

	e.lines[at.line-1] = line[:start] + quote(version, at.style) + line[start+len(old):]
	at.value = version
	e.at[service] = at
	return nil
}

// Save writes the file back with the permissions it had.
func (e *Editor) Save() error {
	return os.WriteFile(e.path, []byte(strings.Join(e.lines, "\n")), e.mode)
}

// quote renders a scalar the way it was written. Only the two quoting styles
// can appear on a version; anything else is treated as plain, and Set's guard
// catches it if that assumption was wrong.
func quote(value string, style yaml.Style) string {
	switch style {
	case yaml.SingleQuotedStyle:
		return "'" + value + "'"
	case yaml.DoubleQuotedStyle:
		return `"` + value + `"`
	default:
		return value
	}
}

// document unwraps the document node yaml.Unmarshal produces.
func document(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0]
	}
	return n
}

// child returns the value of key in a mapping node, or nil.
func child(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
