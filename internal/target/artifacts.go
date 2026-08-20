package target

import "errors"

// ErrArtifactsUnknown is returned by Artifacts when a driver cannot say which
// versions exist for a target.
//
// It is not a failure. An image hosted somewhere the tool has no client for —
// Docker Hub, a self-hosted registry — is perfectly deployable; the only thing
// missing is the list. The update command therefore offers every candidate and
// says it could not check, rather than presenting an empty list that looks like
// "nothing was ever built".
var ErrArtifactsUnknown = errors.New("cannot list the artifacts for this target")

// Present filters versions down to the ones in have, keeping the order they
// were given in.
//
// That order is the newest-first order of the git log, and it is what the
// version list is read in, so it survives the filtering rather than being
// re-derived from tag timestamps — which record when an image was pushed, not
// which commit came after which.
func Present(versions []string, have map[string]bool) []string {
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		if have[v] {
			out = append(out, v)
		}
	}
	return out
}
