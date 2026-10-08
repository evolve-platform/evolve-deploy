package config

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// readSchema decodes the published schema as plain JSON. Read from the file
// rather than through the schema package so these tests see exactly what an
// editor sees.
func readSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../schema/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func dig(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("schema has nothing at %s", strings.Join(path, "."))
		}
		v = m[p]
	}
	return v
}

func keysOf(m any) []string {
	var out []string
	for k := range m.(map[string]any) {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// yamlFields lists the keys a struct decodes, which is what the schema has to
// list: the decoder still runs strictly after the schema, so a field the
// schema lacks is refused by one and allowed by the other.
func yamlFields(v any) []string {
	var out []string
	typ := reflect.TypeOf(v)
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestTheSchemaListsEveryField(t *testing.T) {
	doc := readSchema(t)
	tests := []struct {
		name   string
		value  any
		schema any
	}{
		{"the file", File{}, dig(t, doc, "properties")},
		{"cloud", CloudConfig{}, dig(t, doc, "definitions", "cloud", "properties")},
		{"refs", RefConfig{}, dig(t, doc, "definitions", "refs", "properties")},
		{"strategy", Strategy{}, dig(t, doc, "definitions", "strategy", "properties")},
		{"service", Service{}, dig(t, doc, "definitions", "service", "properties")},
		{"target", Target{}, dig(t, doc, "definitions", "target", "properties")},
		{"code", Code{}, dig(t, doc, "definitions", "code", "properties")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := keysOf(tc.schema), yamlFields(tc.value); !slices.Equal(got, want) {
				t.Errorf("schema lists %v, the type decodes %v", got, want)
			}
		})
	}
}

// The schema refuses a target type on the wrong cloud by itself, so its table
// and typesByCloud have to agree.
func TestTheSchemaKnowsWhichTypesEachCloudHas(t *testing.T) {
	doc := readSchema(t)

	var all []string
	for _, v := range dig(t, doc, "definitions", "target-type", "enum").([]any) {
		all = append(all, v.(string))
	}
	var want []string
	for _, types := range typesByCloud {
		for _, ty := range types {
			want = append(want, string(ty))
		}
	}
	sort.Strings(all)
	sort.Strings(want)
	if !slices.Equal(all, want) {
		t.Errorf("schema has target types %v, typesByCloud %v", all, want)
	}

	seen := map[Cloud]bool{}
	for _, entry := range dig(t, doc, "allOf").([]any) {
		cloud := Cloud(dig(t, entry, "if", "properties", "cloud", "properties", "provider", "const").(string))
		seen[cloud] = true
		svc := dig(t, entry, "then", "properties", "services", "additionalProperties", "properties")
		for _, enum := range []any{
			dig(t, svc, "type", "enum"),
			dig(t, svc, "targets", "items", "properties", "type", "enum"),
		} {
			var got []string
			for _, v := range enum.([]any) {
				got = append(got, v.(string))
			}
			if want := typesOn(cloud); !slices.Equal(got, want) {
				t.Errorf("%s: schema allows %v, typesByCloud %v", cloud, got, want)
			}
		}
	}
	for cloud := range typesByCloud {
		if !seen[cloud] {
			t.Errorf("schema says nothing about the types on %s", cloud)
		}
	}
}

func typesOn(c Cloud) []string {
	var out []string
	for _, t := range typesByCloud[c] {
		out = append(out, string(t))
	}
	return out
}
