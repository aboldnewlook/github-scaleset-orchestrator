package worker

import (
	"fmt"
	"regexp"
	"strings"
)

// MatchRepo reports whether repo ("owner/name") matches any pattern. A pattern
// is "owner/*" or "owner/repo", compared case-insensitively. The owner is never
// a wildcard, and an empty list matches nothing: the allowlist fails closed.
func MatchRepo(patterns []string, repo string) bool {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return false
	}
	for _, p := range patterns {
		po, pn, ok := splitPattern(strings.TrimSpace(p))
		if !ok || !asciiEqualFold(po, owner) {
			continue
		}
		if pn == "*" || asciiEqualFold(pn, name) {
			return true
		}
	}
	return false
}

// splitRepo splits a concrete "owner/name"; both parts must be plain GitHub
// names, so "*" and non-ASCII look-alikes are not repositories.
func splitRepo(s string) (owner, name string, ok bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || !validRepoPart(parts[0]) || !validRepoPart(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// splitPattern splits "owner/name" or "owner/*". The owner is never a wildcard.
func splitPattern(s string) (owner, name string, ok bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || !validRepoPart(parts[0]) {
		return "", "", false
	}
	if parts[1] != "*" && !validRepoPart(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// MaxOwnerScopes caps the allowlist entries a router seeds at enrollment.
const MaxOwnerScopes = 50

var (
	patternOwner = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	patternRepo  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// ValidPattern reports whether p is exactly "owner/*" or "owner/repo": owner is
// ASCII [A-Za-z0-9-], repo is ASCII [A-Za-z0-9._-], and nothing else (no
// whitespace, no other wildcard) is allowed. The error names the entry.
func ValidPattern(p string) error {
	owner, name, ok := strings.Cut(p, "/")
	if ok && patternOwner.MatchString(owner) && (name == "*" || patternRepo.MatchString(name)) {
		return nil
	}
	return fmt.Errorf("invalid allowlist entry %q: want owner/* or owner/repo (owner [A-Za-z0-9-], repo [A-Za-z0-9._-])", p)
}

// ValidatePatterns returns the first invalid entry's ValidPattern error.
func ValidatePatterns(patterns []string) error {
	for _, p := range patterns {
		if err := ValidPattern(p); err != nil {
			return err
		}
	}
	return nil
}
