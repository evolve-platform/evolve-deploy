// Package tmpl renders the small templates that appear in a deploy config:
// hook command lines and Lambda object keys.
package tmpl

import (
	"fmt"
	"strings"
	"text/template"
)

// Render expands {{.version}} and friends against a flat set of values.
func Render(s string, vars map[string]string) (string, error) {
	data := make(map[string]any, len(vars))
	for k, v := range vars {
		data[k] = v
	}
	return RenderWith(s, data, nil)
}

// RenderWith is Render plus the functions a template may call.
//
// Those exist for one reason: a smoke test covering a whole release has no
// single URL to be handed, so it names a service instead. As a function rather
// than a field, because Go template fields have to be identifiers and most
// service names have a hyphen in them — {{.catalog-commercetools.url}} does not
// even parse. One form that always works beats two where one of them is a trap.
//
// missingkey=error matters: a hook that silently becomes
// "hive schema:publish --commit <nothing>" would look like it worked.
func RenderWith(s string, data any, funcs template.FuncMap) (string, error) {
	t, err := template.New("").Funcs(funcs).Option("missingkey=error").Parse(s)
	if err != nil {
		return "", fmt.Errorf("%q: %w", s, explainParse(err, s))
	}
	var out strings.Builder
	if err := t.Execute(&out, data); err != nil {
		return "", fmt.Errorf("%q: %w", s, err)
	}
	return out.String(), nil
}

// explainParse turns one cryptic template error into a usable one.
//
// Writing a service name as a field is the obvious first guess and it does not
// parse, because a template field has to be an identifier. What Go says about
// it is `bad character U+002D '-'`, which tells you nothing about what to write
// instead.
func explainParse(err error, _ string) error {
	if !strings.Contains(err.Error(), "U+002D") {
		return err
	}
	return fmt.Errorf(
		`%w — a service is named in a function argument, not as a field: `+
			`write {{url "some-service"}}, not {{.some-service.url}}`, err)
}
