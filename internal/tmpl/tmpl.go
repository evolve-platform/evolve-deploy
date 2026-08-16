// Package tmpl renders the small templates that appear in a deploy config:
// hook command lines and Lambda object keys.
package tmpl

import (
	"fmt"
	"strings"
	"text/template"
)

// Render expands {{.version}} and friends.
//
// missingkey=error matters: a hook that silently becomes
// "hive schema:publish --commit <nothing>" would look like it worked.
func Render(s string, vars map[string]string) (string, error) {
	t, err := template.New("").Option("missingkey=error").Parse(s)
	if err != nil {
		return "", fmt.Errorf("%q: %w", s, err)
	}
	var out strings.Builder
	if err := t.Execute(&out, vars); err != nil {
		return "", fmt.Errorf("%q: %w", s, err)
	}
	return out.String(), nil
}
