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
		po, pn, ok := splitRepo(strings.TrimSpace(p))
		if !ok || !strings.EqualFold(po, owner) {
			continue
		}
		if pn == "*" || strings.EqualFold(pn, name) {
			return true
		}
	}
	return false
}

func splitRepo(s string) (owner, name string, ok bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
