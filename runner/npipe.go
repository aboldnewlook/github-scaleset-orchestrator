package runner

import (
	"fmt"
	"regexp"
	"strings"
)

const npipePrefix = "npipe:////./pipe/"

var pipeNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// namedPipePath turns a Docker named-pipe endpoint (npipe:////./pipe/<name>)
// into the Windows pipe path \\.\pipe\<name>.
func namedPipePath(host string) (string, error) {
	if !strings.HasPrefix(host, npipePrefix) {
		return "", fmt.Errorf("docker host %q: named pipes must look like npipe:////./pipe/<name>", host)
	}
	name := strings.TrimPrefix(host, npipePrefix)
	if !pipeNamePattern.MatchString(name) {
		return "", fmt.Errorf("docker host %q: invalid pipe name", host)
	}
	return `\\.\pipe\` + name, nil
}
