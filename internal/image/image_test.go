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
		// Nor is the colon in a digest. This is the form a Cloud Run revision
		// reports whatever Terraform declared, so it is read far more often
		// than it is written.
		{"europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase@sha256:17df9cfe6dfdead3dcc29cede6d8480adefe0b2fa3dccbbf6510655f45657900",
			"europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase", ""},
		// Both at once: a tag and the digest it resolved to.
		{"evolvebylabdigital.azurecr.io/purchase:abc1234@sha256:17df9cfe",
			"evolvebylabdigital.azurecr.io/purchase", "abc1234"},
		{"localhost:5000/purchase@sha256:17df9cfe", "localhost:5000/purchase", ""},
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

// The bug this guards against cost a release: a digest-pinned reference retagged
// by naive replacement becomes repo@sha256:<tag>, which is neither a tag nor a
// digest, and Cloud Run only says so once staging is already under way.
func TestRetagDropsADigest(t *testing.T) {
	const ref = "europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase" +
		"@sha256:17df9cfe6dfdead3dcc29cede6d8480adefe0b2fa3dccbbf6510655f45657900"

	got, err := Retag(ref, "27ec167")
	if err != nil {
		t.Fatal(err)
	}
	if want := "europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase:27ec167"; got != want {
		t.Errorf("Retag = %q, want %q", got, want)
	}
}

// A digest names an image without naming a version, so there is no version to
// report. Anything else here ends up displayed as the release that is running.
func TestTagOfADigestIsEmpty(t *testing.T) {
	const ref = "europe-west4-docker.pkg.dev/evolve-mgmt/evolve/purchase" +
		"@sha256:17df9cfe6dfdead3dcc29cede6d8480adefe0b2fa3dccbbf6510655f45657900"

	if got := Tag(ref); got != "" {
		t.Errorf("Tag = %q, want empty", got)
	}
}
