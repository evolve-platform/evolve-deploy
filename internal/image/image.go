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
// It takes care not to mistake the port in a registry host for a tag separator:
// localhost:5000/purchase has no tag.
func Split(ref string) (repo, tag string) {
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// Retag keeps the registry and repository and replaces the tag.
func Retag(ref, version string) (string, error) {
	repo, _ := Split(ref)
	if repo == "" {
		return "", fmt.Errorf("cannot read a repository from image %q", ref)
	}
	return repo + ":" + version, nil
}

// Tag returns just the tag, which is how the currently deployed version is read
// back on every target that runs a container.
func Tag(ref string) string {
	_, tag := Split(ref)
	return tag
}
