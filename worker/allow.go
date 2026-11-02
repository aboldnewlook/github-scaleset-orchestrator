package worker

import "strings"

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
