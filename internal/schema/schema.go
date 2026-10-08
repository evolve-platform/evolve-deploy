// Package schema holds the JSON Schema for deploy/<env>.yaml, and checks a
// file against it before anything decodes it.
//
// The same document is published for editors, so a file is checked as it is
// typed against exactly the rules the tool will apply. That is why the schema
// is the source and not something generated from the Go types: what decides
// whether a file is valid is the rules in it, and a generator only sees the
// fields.
//
// It checks shape — which keys exist where, what type each takes, which apply
// on which cloud and target type. What only makes sense across the whole file
// (a depends_on cycle, two targets with one name, sides naming different
// variables) stays in config, where it can say so properly.
package schema

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// LatestURL is the $id the schema carries in this repository. A release
// publishes it under its own version as well; see URL.
const LatestURL = "https://deploy.evolve-platform.com/schema/latest.json"

//go:embed schema.json
var raw []byte

// URL is where the schema for a release is published. A build that is not a
// release gets the latest, which is the closest thing there is to it.
func URL(version string) string {
	if version == "" || version == "dev" {
		return LatestURL
	}
	return fmt.Sprintf("https://deploy.evolve-platform.com/schema/v%s.json", version)
}

// Document is the schema as it is published at url: the embedded one with its
// $id pointing at where it lives, so a copy saved somewhere still says which
// release it describes.
func Document(url string) []byte {
	return bytes.Replace(raw, []byte(`"$id": "`+LatestURL+`"`), []byte(`"$id": "`+url+`"`), 1)
}

type compiled struct {
	schema *jsonschema.Schema
	// doc is the schema as plain JSON, for what the validator does not
	// carry in its errors: an `if` beside the `then` that failed, the fields
	// an object takes, the message a type error should give.
	doc any
}

var load = sync.OnceValues(func() (*compiled, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(LatestURL, doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(LatestURL)
	if err != nil {
		return nil, err
	}
	// The validator's copy holds json.Number and its own types; a second,
	// plain decode is simpler to walk.
	var plain any
	if err := json.Unmarshal(raw, &plain); err != nil {
		return nil, err
	}
	return &compiled{schema: s, doc: plain}, nil
})

// Check reports everything about doc that the schema refuses, one message per
// mistake, each led by the path that names it and the line it is on. Nothing
// to report is an empty slice.
func Check(doc *yaml.Node) []string {
	c, err := load()
	if err != nil {
		// The schema is embedded and the tests compile it, so this is a
		// broken build rather than a broken config.
		panic(fmt.Sprintf("schema: the embedded schema does not compile: %v", err))
	}

	in := toInstance(doc)
	msgs := in.problems

	var verr *jsonschema.ValidationError
	if err := c.schema.Validate(in.value); errors.As(err, &verr) {
		msgs = append(msgs, c.explain(in, verr)...)
	} else if err != nil {
		msgs = append(msgs, err.Error())
	}

	sort.Strings(msgs)
	return slices.Compact(msgs)
}
