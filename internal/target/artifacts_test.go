package target

import (
	"slices"
	"testing"
)

// The order of the input is the newest-first order of the git log, and the
// filtered list has to keep it: a registry's own order is push time, which is
// not the order the commits were made in.
func TestPresentKeepsTheOrderItWasGiven(t *testing.T) {
	versions := []string{"aaa1111", "bbb2222", "ccc3333", "ddd4444"}
	have := map[string]bool{"ddd4444": true, "bbb2222": true, "eee5555": true}

	got := Present(versions, have)
	want := []string{"bbb2222", "ddd4444"}
	if !slices.Equal(got, want) {
		t.Errorf("Present() = %v, want %v", got, want)
	}
}

func TestPresentWithNothingAvailable(t *testing.T) {
	got := Present([]string{"aaa1111"}, map[string]bool{})
	if len(got) != 0 {
		t.Errorf("Present() = %v, want empty", got)
	}
}
