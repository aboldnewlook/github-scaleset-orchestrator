package worker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
		GitHubURL string `json:"gitHubUrl"` // encoding/json matches keys case-insensitively
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
	if u.Scheme != "https" || info.Host != "github.com" || u.Port() != "" {
		return info, fmt.Errorf("%w (got %s://%s)", ErrJITHost, u.Scheme, u.Host)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return info, fmt.Errorf("%w: gitHubUrl is not a repository URL", ErrJITMalformed)
	}
	info.Repo = parts[0] + "/" + parts[1]
	return info, nil
}

// OfferCheck is what a JIT config must agree with before launch.
type OfferCheck struct {
	Repo         string   // Offer.repo
	Public       bool     // Offer.public_repo
	AcceptPublic bool     // the worker's effective local accept-public
	Allowlist    []string // the worker-local allowlist (owner/* or owner/repo)
}

// VerifyJIT parses a JIT config and checks it against the offer and the
// worker's own policy (design spec §8.3). Any failure means: reject the offer.
func VerifyJIT(encoded string, c OfferCheck) (JITInfo, error) {
	info, err := ParseJIT(encoded)
	if err != nil {
		return info, err
	}
	if !strings.EqualFold(info.Repo, c.Repo) {
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
