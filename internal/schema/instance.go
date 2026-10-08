package schema

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// place is where a value sits in the file: the path a message names it by, and
// the line an editor can be sent to.
type place struct {
	path string
	line int
}

// instance is a YAML document turned into what a JSON Schema validator reads,
// with the place of every value kept beside it.
type instance struct {
	value  any
	places map[string]place
	// problems are what the conversion itself found: things that are wrong
	// with the YAML before any schema has a say, such as a key set twice.
	problems []string
}

// toInstance converts a document node.
//
// Walked by hand rather than decoded into an `any`, for two reasons. The
// validator only reports where a value is, and the line it is on lives on the
// node. And yaml's own idea of an untyped scalar is not the one the config
// decoder applies: decoded into `any`, `version: 1.10` is the float 1.1 and
// `2024-01-01` a time, where the string field it lands in gets the text as
// written. The schema has to judge what the decoder will see.
func toInstance(doc *yaml.Node) *instance {
	in := &instance{places: map[string]place{}}
	n := doc
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	in.value = in.convert(n, nil, "")
	return in
}

// key is how a location is looked up in places. The validator hands back the
// same tokens, so the separator only has to be something no key contains.
func key(loc []string) string { return strings.Join(loc, "\x1f") }

func (in *instance) convert(n *yaml.Node, loc []string, path string) any {
	// An alias is the anchored value written somewhere else. It is reported
	// at the anchor, which is the only place there is text to fix.
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	in.places[key(loc)] = place{path: path, line: n.Line}

	switch n.Kind {
	case yaml.MappingNode:
		return in.mapping(n, loc, path)
	case yaml.SequenceNode:
		out := make([]any, len(n.Content))
		for i, item := range n.Content {
			out[i] = in.convert(item, append(clone(loc), strconv.Itoa(i)), fmt.Sprintf("%s[%d]", path, i))
		}
		return out
	default:
		return scalar(n)
	}
}

func (in *instance) mapping(n *yaml.Node, loc []string, path string) map[string]any {
	out := map[string]any{}
	set := map[string]int{}

	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]

		// `<<: *defaults` lays another mapping under this one. Keys written
		// here win, which is what the decoder does too, so the merged ones
		// only fill what is missing.
		if k.Tag == "!!merge" {
			for _, m := range merged(v) {
				for name, value := range in.mapping(m, loc, path) {
					if _, ok := out[name]; !ok {
						out[name] = value
					}
				}
			}
			continue
		}

		name := k.Value
		child := name
		if path != "" {
			child = path + "." + name
		}
		if line, ok := set[name]; ok {
			in.problems = append(in.problems, fmt.Sprintf(
				"%s: line %d: already set on line %d", child, k.Line, line))
			continue
		}
		set[name] = k.Line

		// An empty value decodes to the field's zero value, which is the same
		// as leaving the key out. `after:` with every entry commented out is a
		// normal thing to find in a file, and it has always meant no hooks.
		if isNull(v) {
			continue
		}
		out[name] = in.convert(v, append(clone(loc), name), child)
	}
	return out
}

// merged is what a merge key points at: one mapping, or a list of them.
func merged(v *yaml.Node) []*yaml.Node {
	for v.Kind == yaml.AliasNode && v.Alias != nil {
		v = v.Alias
	}
	switch v.Kind {
	case yaml.MappingNode:
		return []*yaml.Node{v}
	case yaml.SequenceNode:
		var out []*yaml.Node
		for _, item := range v.Content {
			out = append(out, merged(item)...)
		}
		return out
	}
	return nil
}

func isNull(n *yaml.Node) bool {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// scalar types a value the way the validator needs to tell a list from a
// number from a string. Anything that is not plainly a bool or a number is the
// text as written, timestamps included.
func scalar(n *yaml.Node) any {
	switch n.Tag {
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return b
		}
	case "!!int":
		var i int64
		if n.Decode(&i) == nil {
			return i
		}
	case "!!float":
		var f float64
		if n.Decode(&f) == nil {
			return f
		}
	}
	return n.Value
}

func clone(loc []string) []string { return append([]string(nil), loc...) }
