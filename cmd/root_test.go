package cmd

import (
	"strings"
	"testing"
)

func TestHookVars(t *testing.T) {
	got, err := hookVars([]string{"graph=evolve-tst-blue", "ticket=EVO-123"})
	if err != nil {
		t.Fatal(err)
	}
	if got["graph"] != "evolve-tst-blue" || got["ticket"] != "EVO-123" {
		t.Errorf("vars = %v", got)
	}

	// A value may contain an = of its own; only the first one separates.
	if got, err := hookVars([]string{"endpoint=https://x/?a=b"}); err != nil {
		t.Fatal(err)
	} else if got["endpoint"] != "https://x/?a=b" {
		t.Errorf("endpoint = %q", got["endpoint"])
	}
}

// Shadowing one of the tool's own names would mean a hook receiving a different
// version than the one being deployed.
func TestHookVarsRefusesReservedNames(t *testing.T) {
	for _, name := range []string{"version", "label", "previous_label", "url", "env"} {
		if _, err := hookVars([]string{name + "=x"}); err == nil {
			t.Errorf("--var %s= was accepted", name)
		} else if !strings.Contains(err.Error(), "the tool's own") {
			t.Errorf("--var %s: error = %v", name, err)
		}
	}
}

func TestHookVarsRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"noequals", "=value"} {
		if _, err := hookVars([]string{bad}); err == nil {
			t.Errorf("--var %q was accepted", bad)
		}
	}
}
