package naming

import (
	"fmt"
	"strings"
)

// ScaleSetNamePrefix is the prefix used for all gso-managed scale set names.
const ScaleSetNamePrefix = "gso"

// RepoShortName extracts the repository name from an "owner/repo" string.
// If the input does not contain a slash it is returned as-is.
func RepoShortName(repo string) string {
	parts := strings.Split(repo, "/")
	if len(parts) == 2 {
		return parts[1]
	}
	return repo
}

// ScaleSetName builds the canonical scale set name for a given hostname and
// repository. The format is "<prefix>-<hostname>-<repoShortName>".
func ScaleSetName(prefix, hostname, repo string) string {
	return fmt.Sprintf("%s-%s-%s", prefix, hostname, RepoShortName(repo))
}
