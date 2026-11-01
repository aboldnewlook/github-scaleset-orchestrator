package worker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// encodeJIT builds a JIT config in the actions/runner format: base64 of a JSON
// map from runner file name to base64 file content (design spec §8.3).
func encodeJIT(t *testing.T, runnerJSON string, enc *base64.Encoding) string {
	t.Helper()
	files := map[string]string{
		".runner":      base64.StdEncoding.EncodeToString([]byte(runnerJSON)),
		".credentials": base64.StdEncoding.EncodeToString([]byte("{}")),
	}
	data, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	return enc.EncodeToString(data)
}

func runnerFile(gitHubURL string) string {
	return `{"agentId":7,"agentName":"gso-mac-prune-a1b2c3","poolId":1,"serverUrl":"https://pipelines.actions.githubusercontent.com/x","gitHubUrl":"` + gitHubURL + `","workFolder":"_work"}`
}

func TestParseJIT(t *testing.T) {
	noRunner, _ := json.Marshal(map[string]string{".credentials": base64.StdEncoding.EncodeToString([]byte("{}"))})
	tests := []struct {
		name     string
		encoded  string
		wantRepo string
		wantErr  error
	}{
		{"good", encodeJIT(t, runnerFile("https://github.com/jrschumacher/prune"), base64.StdEncoding), "jrschumacher/prune", nil},
		{"unpadded outer base64", encodeJIT(t, runnerFile("https://github.com/o/r"), base64.RawStdEncoding), "o/r", nil},
		{"BOM in .runner", encodeJIT(t, "\xef\xbb\xbf"+runnerFile("https://github.com/o/r"), base64.StdEncoding), "o/r", nil},
		{"capitalised key", encodeJIT(t, `{"GitHubUrl":"https://github.com/o/r"}`, base64.StdEncoding), "o/r", nil},
		{"trailing slash", encodeJIT(t, runnerFile("https://github.com/o/r/"), base64.StdEncoding), "o/r", nil},
		{"other host", encodeJIT(t, runnerFile("https://evil.example/o/r"), base64.StdEncoding), "", ErrJITHost},
		{"http scheme", encodeJIT(t, runnerFile("http://github.com/o/r"), base64.StdEncoding), "", ErrJITHost},
		{"explicit port", encodeJIT(t, runnerFile("https://github.com:8443/o/r"), base64.StdEncoding), "", ErrJITHost},
		{"lookalike host", encodeJIT(t, runnerFile("https://github.com.evil.example/o/r"), base64.StdEncoding), "", ErrJITHost},
		{"owner only", encodeJIT(t, runnerFile("https://github.com/o"), base64.StdEncoding), "", ErrJITMalformed},
		{"no gitHubUrl", encodeJIT(t, `{"agentName":"x"}`, base64.StdEncoding), "", ErrJITMalformed},
		{"no .runner file", base64.StdEncoding.EncodeToString(noRunner), "", ErrJITMalformed},
		{"not base64", "not base64 !!", "", ErrJITMalformed},
		{"base64 but not JSON", base64.StdEncoding.EncodeToString([]byte("hello")), "", ErrJITMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ParseJIT(tt.encoded)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ParseJIT() error = %v, want %v", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), tt.encoded) {
					t.Fatal("error message contains the encoded JIT config")
				}
				return
			}
			if err != nil || info.Repo != tt.wantRepo || info.Host != "github.com" {
				t.Fatalf("ParseJIT() = %+v, %v; want repo %s", info, err, tt.wantRepo)
			}
		})
	}
}

func TestVerifyJIT(t *testing.T) {
	good := encodeJIT(t, runnerFile("https://github.com/jrschumacher/prune"), base64.StdEncoding)
	allow := []string{"jrschumacher/*"}
	tests := []struct {
		name    string
		encoded string
		check   OfferCheck
		wantErr error
	}{
		{"matches offer and allowlist", good, OfferCheck{Repo: "jrschumacher/prune", Allowlist: allow}, nil},
		{"case-insensitive repo match", good, OfferCheck{Repo: "JRSchumacher/Prune", Allowlist: allow}, nil},
		{"offer names another repo", good, OfferCheck{Repo: "jrschumacher/other", Allowlist: allow}, ErrJITRepo},
		{"repo not on the allowlist", good, OfferCheck{Repo: "jrschumacher/prune", Allowlist: []string{"aboldnewlook/*"}}, ErrJITNotAllowed},
		{"empty allowlist", good, OfferCheck{Repo: "jrschumacher/prune"}, ErrJITNotAllowed},
		{"public without local accept", good, OfferCheck{Repo: "jrschumacher/prune", Public: true, Allowlist: allow}, ErrJITPublic},
		{"public with local accept", good, OfferCheck{Repo: "jrschumacher/prune", Public: true, AcceptPublic: true, Allowlist: allow}, nil},
		{"malformed config", "garbage", OfferCheck{Repo: "jrschumacher/prune", Allowlist: allow}, ErrJITMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := VerifyJIT(tt.encoded, tt.check)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("VerifyJIT() error = %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("VerifyJIT() error = %v, want %v", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), tt.encoded) {
				t.Fatal("error message contains the encoded JIT config")
			}
		})
	}
}
