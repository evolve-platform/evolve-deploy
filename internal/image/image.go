// Package image handles container image references.
//
// The registry and repository never appear in a deploy config: they come from
// whatever is already deployed, and only the tag is replaced. Terraform decides
// where images live, evolve-deploy decides which one runs.
package image

import (
	"fmt"
	"strings"
)

// Split separates repository from tag.
//
// A reference is [registry[:port]/]repo-path[:tag][@digest], which puts two
// colons in it that are not the tag's. The port in a registry host is the
// obvious one: localhost:5000/purchase has no tag. The other is the algorithm
// in a digest, and it is the one that bites — purchase@sha256:9ca1640 read as
// repo `purchase@sha256` and tag `9ca1640` is wrong in a way that stays quiet
// until Retag hands the result to a cloud. Both are handled here, once, rather
// than at each of the dozen call sites.
func Split(ref string) (repo, tag string) {
	// The digest comes off first, so its colon cannot be taken for the tag's.
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// Retag keeps the registry and repository and replaces the tag.
//
// A digest is dropped rather than kept beside the new tag, and on Cloud Run
// that is the whole point: a revision records the image as the digest the tag
// resolved to at the time, never as it was written, so the reference read back
// from a serving revision is pinned even when Terraform declared a tag. Writing
// repo@sha256:<tag> is what a naive replacement produces, and Cloud Run rejects
// it several seconds into a release. The tag is the pin this tool moves; a
// digest recovered from the cloud is not something anyone asked for.
func Retag(ref, version string) (string, error) {
	repo, _ := Split(ref)
	if repo == "" {
		return "", fmt.Errorf("cannot read a repository from image %q", ref)
	}
	return repo + ":" + version, nil
}

// Tag returns just the tag, which is how the currently deployed version is read
// back on every target that runs a container.
//
// It is empty for a reference pinned by digest, which is honest rather than
// unhelpful: a digest names an image without naming a version, and the hex on
// its own is not one. A caller that gets nothing back reports the running
// version as unknown and deploys, which is the safe direction — the other one
// is a 64-character hash presented as a release.
func Tag(ref string) string {
	_, tag := Split(ref)
	return tag
}
