package worker

import "regexp"

// runnerNamePattern is what a runner name may look like. The router mints
// "gso-<worker segment>-<repo>-<6 chars>" from [a-z0-9-] (internal/naming), at
// most about 40 characters. The worker uses the name in file paths, so it
// accepts nothing wider.
var runnerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// validRunnerName reports whether name is safe to use as a file name and a
// container name.
func validRunnerName(name string) bool { return runnerNamePattern.MatchString(name) }
