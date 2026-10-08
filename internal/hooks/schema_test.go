package hooks

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// options is the type each action decodes its `with` into. Listed by hand
// because the plugin table holds parsers, not types; the first check below is
// what stops it falling behind that table.
var options = map[string]any{
	"cloud-run-job":     jobOptions{},
	"container-app-job": jobOptions{},
	"ecs-task":          ecsTaskOptions{},
	"honeycomb":         honeycombOptions{},
	"http":              httpOptions{},
	"sentry":            sentryOptions{},
}

// The schema refuses a `uses` or an option before the parser ever sees it, so
// an action added here and not there could never be used.
func TestTheSchemaKnowsEveryAction(t *testing.T) {
	raw, err := os.ReadFile("../schema/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Definitions map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	names := slices.Sorted(maps.Keys(plugins))
	if got := slices.Sorted(maps.Keys(options)); !slices.Equal(got, names) {
		t.Fatalf("this test lists %v, the plugin table %v", got, names)
	}
	if got := doc.Definitions["hook-block"].Properties["uses"].Enum; !slices.Equal(got, names) {
		t.Errorf("schema allows uses: %v, the plugin table has %v", got, names)
	}

	for _, name := range names {
		def, ok := doc.Definitions[name+"-options"]
		if !ok {
			t.Errorf("schema has no %s-options", name)
			continue
		}
		got := slices.Sorted(maps.Keys(def.Properties))
		if want := fields(options[name]); !slices.Equal(got, want) {
			t.Errorf("%s: schema lists %v, the options decode %v", name, got, want)
		}
	}
}

func fields(v any) []string {
	var out []string
	typ := reflect.TypeOf(v)
	for i := range typ.NumField() {
		name, flags, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		if flags == "inline" {
			out = append(out, fields(reflect.New(typ.Field(i).Type).Elem().Interface())...)
			continue
		}
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
