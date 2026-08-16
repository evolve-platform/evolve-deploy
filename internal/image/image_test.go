package image

import "testing"

func TestSplit(t *testing.T) {
	tests := []struct {
		in        string
		repo, tag string
	}{
		{"1234.dkr.ecr.eu-west-1.amazonaws.com/purchase:abc1234",
			"1234.dkr.ecr.eu-west-1.amazonaws.com/purchase", "abc1234"},
		{"evolvebylabdigital.azurecr.io/purchase:abc1234",
			"evolvebylabdigital.azurecr.io/purchase", "abc1234"},
		{"europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase:abc1234",
			"europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase", "abc1234"},
		// A port in the registry host is not a tag separator.
		{"localhost:5000/purchase", "localhost:5000/purchase", ""},
		{"localhost:5000/purchase:v1", "localhost:5000/purchase", "v1"},
		{"purchase", "purchase", ""},
	}
	for _, tc := range tests {
		repo, tag := Split(tc.in)
		if repo != tc.repo || tag != tc.tag {
			t.Errorf("Split(%q) = (%q, %q), want (%q, %q)", tc.in, repo, tag, tc.repo, tc.tag)
		}
	}
}

func TestRetagKeepsTheRegistry(t *testing.T) {
	// The registry never appears in the deploy config: Terraform decides where
	// images live, this tool only decides which tag runs.
	got, err := Retag("evolvebylabdigital.azurecr.io/purchase:bootstrap", "abc1234")
	if err != nil {
		t.Fatal(err)
	}
	if want := "evolvebylabdigital.azurecr.io/purchase:abc1234"; got != want {
		t.Errorf("Retag = %q, want %q", got, want)
	}
}
