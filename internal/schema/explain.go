package schema

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// explain turns the validator's tree of errors into one line per mistake.
//
// The validator's own wording is about the schema — "additionalProperties
// 'comand' not allowed", "allOf failed", "false schema" — and names locations
// as JSON pointers. Someone fixing a YAML file has neither in front of them.
// Each message here says what is wrong in the terms of the file, under the path
// the rest of the tool's validation uses, with the line beside it.
func (c *compiled) explain(in *instance, err *jsonschema.ValidationError) []string {
	var msgs []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		// Only the leaves say anything. The nodes above them are the
		// schema's structure — a $ref followed, an allOf entered — and
		// would repeat the leaf in worse words.
		if len(e.Causes) > 0 {
			for _, cause := range e.Causes {
				walk(cause)
			}
			return
		}
		msgs = append(msgs, c.leaf(in, e)...)
	}
	walk(err)
	return msgs
}

func (c *compiled) leaf(in *instance, e *jsonschema.ValidationError) []string {
	here := in.places[key(e.InstanceLocation)]
	at := c.at(e.SchemaURL)
	when := c.condition(e.SchemaURL)

	switch k := e.ErrorKind.(type) {
	case *kind.AdditionalProperties:
		// One message per key, at the key: the path then names the thing to
		// delete or rename, and the line is the line it is on.
		noun := "field"
		if strings.HasSuffix(str(at["title"]), " options") {
			noun = "option"
		}
		allowed := sortedKeys(at["properties"])
		var out []string
		for _, p := range k.Properties {
			loc := append(clone(e.InstanceLocation), p)
			msg := "unknown " + noun
			if s := suggest(p, allowed); s != "" {
				msg += fmt.Sprintf(" (did you mean %s?)", s)
			} else {
				msg += fmt.Sprintf(" (want one of %s)", strings.Join(allowed, ", "))
			}
			out = append(out, line(in.places[key(loc)], msg))
		}
		return out

	case *kind.Required:
		// The key is not there to point at, so the path names it and the line
		// is the block it belongs in.
		out := make([]string, 0, len(k.Missing))
		for _, m := range k.Missing {
			msg := "required"
			if when != "" {
				msg += " when " + when
			}
			out = append(out, line(place{path: join(here.path, m), line: here.line}, msg))
		}
		return out

	case *kind.FalseSchema:
		// Every false in this schema switches a field off for a provider or a
		// target type, so the `if` beside it is the reason.
		if when != "" {
			return []string{line(here, "does not apply when "+when)}
		}
		return []string{line(here, "is not allowed here")}

	case *kind.Type:
		got := describe(resolve(in.value, e.InstanceLocation))
		if custom := str(at["errorMessage"]); custom != "" {
			return []string{line(here, fmt.Sprintf("%s, not %s", custom, got))}
		}
		msg := fmt.Sprintf("want %s, got %s", want(k.Want), got)
		// The one that is nearly always meant: a single name where a list
		// goes. YAML has a short way to write a list of one, and saying so
		// is quicker than explaining what a sequence is.
		if slices.Contains(k.Want, "array") {
			if s, ok := resolve(in.value, e.InstanceLocation).(string); ok {
				msg += fmt.Sprintf("; a list of one is written [%s]", s)
			}
		}
		return []string{line(here, msg)}

	case *kind.Enum:
		got := describe(k.Got)
		options := make([]string, len(k.Want))
		for i, w := range k.Want {
			options[i] = fmt.Sprint(w)
		}
		var msg string
		if when != "" {
			msg = fmt.Sprintf("%s is not valid when %s (want one of %s)", got, when, strings.Join(options, ", "))
		} else {
			msg = fmt.Sprintf("%s is not one of %s", got, strings.Join(options, ", "))
		}
		if s, ok := k.Got.(string); ok {
			if guess := suggest(s, options); guess != "" {
				msg += fmt.Sprintf(" (did you mean %s?)", guess)
			}
		}
		return []string{line(here, msg)}

	case *kind.MinProperties:
		return []string{line(here, entries("needs at least", k.Want))}

	case *kind.MinItems:
		return []string{line(here, entries("needs at least", k.Want))}
	}

	// A keyword this schema does not use yet. The validator's sentence is
	// better than nothing, and a test that hits it is the prompt to add a case.
	return []string{line(here, e.ErrorKind.LocalizedString(message.NewPrinter(language.English)))}
}

// line puts a message together the way every config error is shaped:
// path, then line, then what is wrong.
func line(p place, msg string) string {
	if p.path == "" {
		return fmt.Sprintf("line %d: %s", p.line, msg)
	}
	return fmt.Sprintf("%s: line %d: %s", p.path, p.line, msg)
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// at returns the schema object a URL from the validator points at.
func (c *compiled) at(schemaURL string) map[string]any {
	node, _ := resolve(c.doc, pointer(schemaURL)).(map[string]any)
	return node
}

// condition says why the rule an error came from applies: the `if` beside the
// nearest enclosing `then`, as "type is lambda" or "cloud.provider is aws".
//
// Without it a refusal reads as arbitrary. `cluster` is a perfectly good field;
// what is wrong is having it on a lambda, and the message has to say so.
func (c *compiled) condition(schemaURL string) string {
	segs := pointer(schemaURL)
	i := -1
	for j := len(segs) - 1; j >= 0; j-- {
		if segs[j] == "then" {
			i = j
			break
		}
	}
	if i < 0 {
		return ""
	}
	cond, _ := resolve(c.doc, append(clone(segs[:i]), "if")).(map[string]any)
	return describeIf(cond, "")
}

// describeIf reads the conditions this schema writes — a property, perhaps
// nested, that has to equal a value — back into words.
func describeIf(cond map[string]any, prefix string) string {
	props, _ := cond["properties"].(map[string]any)
	for _, name := range sortedKeys(props) {
		sub, _ := props[name].(map[string]any)
		switch {
		case sub["const"] != nil:
			return fmt.Sprintf("%s%s is %v", prefix, name, sub["const"])
		case sub["enum"] != nil:
			var vals []string
			for _, v := range sub["enum"].([]any) {
				vals = append(vals, fmt.Sprint(v))
			}
			return fmt.Sprintf("%s%s is %s", prefix, name, strings.Join(vals, " or "))
		case sub["properties"] != nil:
			if s := describeIf(sub, prefix+name+"."); s != "" {
				return s
			}
		}
	}
	return ""
}

// pointer splits the fragment of a schema URL into its tokens.
func pointer(schemaURL string) []string {
	_, frag, _ := strings.Cut(schemaURL, "#")
	frag, _ = url.PathUnescape(frag)
	frag = strings.TrimPrefix(frag, "/")
	if frag == "" {
		return nil
	}
	segs := strings.Split(frag, "/")
	for i, s := range segs {
		segs[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
	}
	return segs
}

// resolve follows tokens through decoded JSON, for the schema and the
// instance alike.
func resolve(v any, tokens []string) any {
	for _, t := range tokens {
		switch x := v.(type) {
		case map[string]any:
			v = x[t]
		case []any:
			i, err := strconv.Atoi(t)
			if err != nil || i < 0 || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

// want names what a type error expected, in words that fit YAML.
func want(types []string) string {
	has := func(t string) bool { return slices.Contains(types, t) }
	var words []string
	// Every plain value field takes all three, and "a string or a number or
	// a boolean" is a long way of saying "not a list or a block".
	if has("string") && has("number") && has("boolean") {
		words = append(words, "a single value")
	} else {
		for _, t := range types {
			switch t {
			case "string":
				words = append(words, "text")
			case "number":
				words = append(words, "a number")
			case "integer":
				words = append(words, "a whole number")
			case "boolean":
				words = append(words, "true or false")
			}
		}
	}
	if has("array") {
		words = append(words, "a list")
	}
	if has("object") {
		words = append(words, "a block")
	}
	return strings.Join(words, " or ")
}

// describe names a value the way a message mentions it: text quoted so its
// edges show, anything bigger by what it is.
func describe(v any) string {
	switch x := v.(type) {
	case nil:
		return "nothing"
	case string:
		return strconv.Quote(x)
	case map[string]any:
		return "a block"
	case []any:
		return "a list"
	default:
		return fmt.Sprint(x)
	}
}

func entries(prefix string, n int) string {
	if n == 1 {
		return prefix + " one entry"
	}
	return fmt.Sprintf("%s %d entries", prefix, n)
}

// suggest finds the option someone most likely meant, or nothing when no
// option is close enough to be worth guessing.
//
// Spelling is compared without case, `_` or `-`, so `dependsOn`, `env_from`
// and `keep-warm` find the key that is really there.
func suggest(got string, options []string) string {
	norm := func(s string) string {
		return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(s))
	}
	best, bestDist := "", -1
	for _, o := range options {
		d := distance(norm(got), norm(o))
		if bestDist < 0 || d < bestDist {
			best, bestDist = o, d
		}
	}
	limit := 1
	if len(got) >= 5 {
		limit = 2
	}
	if best == "" || bestDist > limit {
		return ""
	}
	return best
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func sortedKeys(v any) []string {
	m, _ := v.(map[string]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
