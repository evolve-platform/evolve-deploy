// Package refs parses and resolves the ${param:…} and ${secret:…} references
// that appear as env-var values in a deploy config.
//
// The config never contains a value — only literals that are safe to read in a
// pull request, and references to a store. Whether a reference is handed to the
// platform untouched or read by the tool is a property of the target, not a
// choice in the config: ECS resolves valueFrom itself, Lambda cannot, so on
// Lambda the tool reads it. See DESIGN.md.
package refs

import (
	"context"
	"fmt"
	"strings"
)

// Kind distinguishes a plain value from a reference into a store.
type Kind int

const (
	// Literal is a value written out in the config.
	Literal Kind = iota
	// Param points at the cloud's configuration store: SSM Parameter Store,
	// Parameter/Secret Manager, App Configuration, a ConfigMap.
	Param
	// Secret points at the cloud's secret store: Secrets Manager, Secret
	// Manager, Key Vault, a Kubernetes Secret.
	Secret
)

func (k Kind) String() string {
	switch k {
	case Param:
		return "param"
	case Secret:
		return "secret"
	default:
		return "literal"
	}
}

// Value is one parsed env-var value.
type Value struct {
	Kind Kind
	// Name is the store key, for Param and Secret.
	Name string
	// Literal is the value, for Kind == Literal.
	Literal string
	// Raw is the text as written, for diff output and error messages.
	Raw string
}

func (v Value) IsRef() bool { return v.Kind != Literal }

func (v Value) String() string {
	if v.IsRef() {
		return v.Raw
	}
	return v.Literal
}

// Resolver reads from one cloud's stores. Verify is separate from Read so that
// a reference which will be passed through natively can still be checked at
// plan time without the tool ever seeing the value — the difference between
// secretsmanager:DescribeSecret and secretsmanager:GetSecretValue.
type Resolver interface {
	// Verify reports whether the referenced item exists. It must not return the
	// value and should use a metadata-only API where one exists.
	Verify(ctx context.Context, v Value) error
	// Read returns the literal value. Only called for targets that cannot pass
	// a reference through, and only when refs.resolve is allow.
	Read(ctx context.Context, v Value) (string, error)
	// ReadMap returns a JSON object stored under a reference, for envFrom.
	ReadMap(ctx context.Context, v Value) (map[string]string, error)
}

// Substitute expands ${env} before parsing. It runs first so that a reference
// may contain it, as in ${param:/evolve/${env}/purchase/setup}: after expansion
// there is exactly one ${…} left to parse.
func Substitute(s, env string) string {
	return strings.ReplaceAll(s, "${env}", env)
}

// Parse reads one env-var value. A reference must be the entire value —
// "https://${param:host}/x" is rejected — because a partially interpolated
// value could never be handed to the platform untouched, so allowing it would
// silently change whether the tool reads secrets.
func Parse(s string) (Value, error) {
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		if i := strings.Index(s, "${"); i >= 0 {
			return Value{}, fmt.Errorf("a reference must be the whole value, not part of one: %q", s)
		}
		return Value{Kind: Literal, Literal: s, Raw: s}, nil
	}

	inner := s[2 : len(s)-1]
	if strings.Contains(inner, "${") {
		return Value{}, fmt.Errorf("nested reference in %q (is ${env} left unexpanded?)", s)
	}

	scheme, name, ok := strings.Cut(inner, ":")
	if !ok {
		return Value{}, fmt.Errorf("reference %q is missing a scheme, want ${param:…} or ${secret:…}", s)
	}
	if name == "" {
		return Value{}, fmt.Errorf("reference %q has an empty name", s)
	}

	switch scheme {
	case "param":
		return Value{Kind: Param, Name: name, Raw: s}, nil
	case "secret":
		return Value{Kind: Secret, Name: name, Raw: s}, nil
	default:
		return Value{}, fmt.Errorf("unknown reference scheme %q in %q, want param or secret", scheme, s)
	}
}

// ParseRef is Parse but rejects literals. Used for envFrom, where a plain
// string would have no meaning.
func ParseRef(s string) (Value, error) {
	v, err := Parse(s)
	if err != nil {
		return Value{}, err
	}
	if !v.IsRef() {
		return Value{}, fmt.Errorf("envFrom takes a reference, got the literal %q", s)
	}
	return v, nil
}
