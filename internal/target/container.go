package target

import (
	"fmt"
	"strings"
)

// PickContainer chooses which container in a multi-container task carries the
// application image.
//
// Sidecars are normal here: both the ECS and the Container Apps modules put a
// reverse proxy next to the application, and some also run an OpenTelemetry
// collector. Only the application container is retagged and given the
// environment from the deploy config — everything else is copied through
// untouched, because those images and their settings belong to Terraform.
//
// The rules, in order:
//
//  1. An explicit `container:` in the config must match exactly.
//  2. A task with one container needs no configuration at all.
//  3. Otherwise the conventional name for the platform, so the common case of
//     "application plus sidecar" also needs no configuration.
//  4. Otherwise an error naming what is actually there, because guessing which
//     of several containers is the application is how you deploy a proxy image
//     to the wrong place.
func PickContainer(names []string, configured, conventional string) (string, error) {
	if configured != "" {
		for _, name := range names {
			if name == configured {
				return name, nil
			}
		}
		return "", fmt.Errorf("no container named %q (found %s)",
			configured, strings.Join(names, ", "))
	}

	if len(names) == 1 {
		return names[0], nil
	}

	for _, name := range names {
		if name == conventional {
			return name, nil
		}
	}

	if len(names) == 0 {
		return "", fmt.Errorf("no containers found")
	}
	return "", fmt.Errorf(
		"cannot tell which of %s is the application container — set `container:` on this target",
		strings.Join(names, ", "))
}
