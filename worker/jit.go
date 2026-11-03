package worker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Errors returned by ParseJIT and VerifyJIT. Every one fails closed: the worker
// rejects the offer and launches nothing (design spec §8.3).
var (
	ErrJITMalformed  = errors.New("JIT config is malformed")
	ErrJITHost       = errors.New("JIT config does not point at https://github.com")
	ErrJITRepo       = errors.New("JIT config repository does not match the offer")
	ErrJITNotAllowed = errors.New("repository is not on this worker's allowlist")
	ErrJITPublic     = errors.New("public repository offered but this worker does not accept public repositories")
)

// JITInfo is what a worker learns from a JIT config before launching it.
type JITInfo struct {
	GitHubURL string // the runner's gitHubUrl, e.g. https://github.com/owner/repo
	Host      string // lowercase host of GitHubURL
	Repo      string // "owner/name"
}

// ParseJIT decodes an encoded JIT config: base64 of a JSON map from runner file
// name to base64 file content. It reads gitHubUrl from the .runner file
// (INFERRED from actions/runner; spike h confirms) and requires
// https://github.com/<owner>/<repo>. Errors never contain the config.
func ParseJIT(encoded string) (JITInfo, error) {
	return parseJIT(encoded, nil)
}

func parseJIT(encoded string, trusted []string) (JITInfo, error) {
	outer, err := decodeBase64(strings.TrimSpace(encoded))
	if err != nil {
		return JITInfo{}, fmt.Errorf("%w: not base64", ErrJITMalformed)
	}
	var files map[string]string
	if err := json.Unmarshal(trimBOM(outer), &files); err != nil {
		return JITInfo{}, fmt.Errorf("%w: not a JSON map of runner files", ErrJITMalformed)
	}
	raw, ok := files[".runner"]
	if !ok {
		return JITInfo{}, fmt.Errorf("%w: no .runner file", ErrJITMalformed)
	}
	runnerJSON, err := decodeBase64(raw)
	if err != nil {
		return JITInfo{}, fmt.Errorf("%w: .runner is not base64", ErrJITMalformed)
	}
	var settings struct {
		GitHubURL   string `json:"gitHubUrl"` // encoding/json matches keys case-insensitively
		ServerURL   string `json:"serverUrl"`
		ServerURLV2 string `json:"serverUrlV2"`
	}
	if err := json.Unmarshal(trimBOM(runnerJSON), &settings); err != nil {
		return JITInfo{}, fmt.Errorf("%w: .runner is not JSON", ErrJITMalformed)
	}
	if settings.GitHubURL == "" {
		return JITInfo{}, fmt.Errorf("%w: .runner has no gitHubUrl", ErrJITMalformed)
	}
	u, err := url.Parse(settings.GitHubURL)
	if err != nil {
		return JITInfo{}, fmt.Errorf("%w: gitHubUrl is not a URL", ErrJITMalformed)
	}
	info := JITInfo{GitHubURL: settings.GitHubURL, Host: strings.ToLower(u.Hostname())}
	if u.User != nil || !gitHubURLTrusted(u, trusted) {
		return info, fmt.Errorf("%w (got %s://%s)", ErrJITHost, u.Scheme, u.Hostname())
	}
	// The runner connects to serverUrl/serverUrlV2 and authenticates there, so
	// a forged gitHubUrl next to a hostile server URL must not pass.
	for _, field := range []struct{ name, value string }{{"serverUrl", settings.ServerURL}, {"serverUrlV2", settings.ServerURLV2}} {
		if field.value == "" {
			continue
		}
		su, err := url.Parse(field.value)
		if err != nil {
			return info, fmt.Errorf("%w: %s is not a URL", ErrJITMalformed, field.name)
		}
		if su.User != nil || !serverURLTrusted(su, trusted) {
			return info, fmt.Errorf("%w: %s is %s://%s", ErrJITHost, field.name, su.Scheme, su.Hostname())
		}
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || !validRepoPart(parts[0]) || !validRepoPart(parts[1]) {
		return info, fmt.Errorf("%w: gitHubUrl is not a repository URL", ErrJITMalformed)
	}
	info.Repo = parts[0] + "/" + parts[1]
	return info, nil
}

var defaultServerSuffixes = []string{"actions.githubusercontent.com", "githubusercontent.com"}

// gitHubURLTrusted: github.com over https with no port, or, with an override,
// a trusted host over https. gitHubUrl stays https-only even with an override:
// the simulator's is always https://github.com/<repo>; only serverUrl may be
// plain http.
func gitHubURLTrusted(u *url.URL, trusted []string) bool {
	if len(trusted) == 0 {
		return u.Scheme == "https" && strings.ToLower(u.Hostname()) == "github.com" && u.Port() == ""
	}
	return u.Scheme == "https" && hostTrusted(u, trusted)
}

func serverURLTrusted(u *url.URL, trusted []string) bool {
	if len(trusted) == 0 {
		return u.Scheme == "https" && hostTrusted(u, defaultServerSuffixes)
	}
	return (u.Scheme == "https" || u.Scheme == "http") && hostTrusted(u, trusted)
}

// hostTrusted reports whether u's host equals an entry (as host or host:port)
// or is a subdomain of one.
func hostTrusted(u *url.URL, entries []string) bool {
	host := strings.ToLower(u.Hostname())
	hostPort := strings.ToLower(u.Host)
	if host == "" {
		return false
	}
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if host == e || hostPort == e || strings.HasSuffix(host, "."+e) {
			return true
		}
	}
	return false
}

var repoPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func validRepoPart(s string) bool { return repoPartPattern.MatchString(s) }

// asciiEqualFold compares with ASCII-only case folding, so U+212A (Kelvin)
// never matches "k".
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// OfferCheck is what a JIT config must agree with before launch.
type OfferCheck struct {
	Repo         string   // Offer.repo
	Public       bool     // Offer.public_repo
	AcceptPublic bool     // the worker's effective local accept-public
	Allowlist    []string // the worker-local allowlist (owner/* or owner/repo)
	// TrustedHosts, when non-empty, replaces the GitHub defaults for both the
	// gitHubUrl host (github.com) and the server URL hosts
	// (*.githubusercontent.com). Entries are a host, host:port, or a domain
	// whose subdomains are trusted. The simulator uses it; production leaves it
	// empty.
	TrustedHosts []string
}

// VerifyJIT parses a JIT config and checks it against the offer and the
// worker's own policy (design spec §8.3). Any failure means: reject the offer.
func VerifyJIT(encoded string, c OfferCheck) (JITInfo, error) {
	info, err := parseJIT(encoded, c.TrustedHosts)
	if err != nil {
		return info, err
	}
	if !asciiEqualFold(info.Repo, c.Repo) {
		return info, fmt.Errorf("%w: offer %q, JIT config %q", ErrJITRepo, c.Repo, info.Repo)
	}
	if !MatchRepo(c.Allowlist, info.Repo) {
		return info, fmt.Errorf("%w: %s", ErrJITNotAllowed, info.Repo)
	}
	if c.Public && !c.AcceptPublic {
		return info, fmt.Errorf("%w: %s", ErrJITPublic, info.Repo)
	}
	return info, nil
}

func decodeBase64(s string) ([]byte, error) {
	var lastErr error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		data, err := enc.DecodeString(s)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func trimBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
}
