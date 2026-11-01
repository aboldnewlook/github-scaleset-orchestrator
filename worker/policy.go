package worker

import (
	"fmt"
	"strings"
)

// PublicMode is the worker-local public-repository setting (design spec §8.4):
// follow the router's pushed default, or override it.
type PublicMode string

const (
	PublicFollow PublicMode = "follow"
	PublicAccept PublicMode = "accept"
	PublicRefuse PublicMode = "refuse"
)

// ParsePublicMode parses follow, accept or refuse (case-insensitive). The empty
// string means follow.
func ParsePublicMode(s string) (PublicMode, error) {
	switch PublicMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", PublicFollow:
		return PublicFollow, nil
	case PublicAccept:
		return PublicAccept, nil
	case PublicRefuse:
		return PublicRefuse, nil
	}
	return "", fmt.Errorf("accept-public must be follow, accept or refuse, got %q", s)
}

// EffectiveAcceptPublic combines the local mode with the router's pushed
// default. Any value other than follow or accept refuses.
func EffectiveAcceptPublic(mode PublicMode, routerDefault bool) bool {
	switch mode {
	case PublicAccept:
		return true
	case PublicFollow, "":
		return routerDefault
	default:
		return false
	}
}
