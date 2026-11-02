package ghsim

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// SimJIT is what a fake runner needs from a simulator JIT config.
type SimJIT struct {
	GitHubURL  string
	ServerURL  string
	RunnerName string
	Token      string
}

// runnerFile mirrors the actions runner's .runner settings file, plus gsoSim.
type runnerFile struct {
	AgentID    int64  `json:"agentId"`
	AgentName  string `json:"agentName"`
	PoolID     int    `json:"poolId"`
	PoolName   string `json:"poolName"`
	ServerURL  string `json:"serverUrl"`
	GitHubURL  string `json:"gitHubUrl"`
	WorkFolder string `json:"workFolder"`
	Ephemeral  bool   `json:"ephemeral"`
	GsoSim     bool   `json:"gsoSim"`
}

type credentialsFile struct {
	Scheme string            `json:"scheme"`
	Data   map[string]string `json:"data"`
}

// EncodeJIT builds a JIT config shaped like the actions runner's (a base64
// JSON map of file name to base64 file content), so worker.VerifyJIT checks it
// exactly as it checks a real one. serverUrl points at the simulator.
func EncodeJIT(gitHubURL, serverURL, runnerName, token string) string {
	runner, _ := json.Marshal(runnerFile{
		AgentName: runnerName, PoolID: 1, PoolName: "Default", ServerURL: serverURL,
		GitHubURL: gitHubURL, WorkFolder: "_work", Ephemeral: true, GsoSim: true,
	})
	creds, _ := json.Marshal(credentialsFile{Scheme: "GsoSim", Data: map[string]string{"token": token}})
	files, _ := json.Marshal(map[string]string{
		".runner":      base64.StdEncoding.EncodeToString(runner),
		".credentials": base64.StdEncoding.EncodeToString(creds),
	})
	return base64.StdEncoding.EncodeToString(files)
}

// DecodeSimJIT reports whether encoded is a simulator JIT config and, if so,
// returns its contents. A well-formed real JIT config returns (zero, false, nil).
// Errors never include the input.
func DecodeSimJIT(encoded string) (SimJIT, bool, error) {
	outer, err := decodeBase64(encoded)
	if err != nil {
		return SimJIT{}, false, errors.New("ghsim: JIT config is not base64")
	}
	var files map[string]string
	if err := json.Unmarshal(outer, &files); err != nil {
		return SimJIT{}, false, errors.New("ghsim: JIT config is not a JSON file map")
	}
	rawRunner, ok := files[".runner"]
	if !ok {
		return SimJIT{}, false, errors.New("ghsim: JIT config has no .runner file")
	}
	runnerJSON, err := decodeBase64(rawRunner)
	if err != nil {
		return SimJIT{}, false, errors.New("ghsim: JIT .runner file is not base64")
	}
	var rf runnerFile
	if err := json.Unmarshal(bytes.TrimPrefix(runnerJSON, []byte("\xef\xbb\xbf")), &rf); err != nil {
		return SimJIT{}, false, errors.New("ghsim: JIT .runner file is not JSON")
	}
	if !rf.GsoSim {
		return SimJIT{}, false, nil
	}
	var cred credentialsFile
	if raw, ok := files[".credentials"]; ok {
		if b, err := decodeBase64(raw); err == nil {
			_ = json.Unmarshal(b, &cred)
		}
	}
	sj := SimJIT{GitHubURL: rf.GitHubURL, ServerURL: rf.ServerURL, RunnerName: rf.AgentName, Token: cred.Data["token"]}
	if sj.RunnerName == "" || sj.Token == "" {
		return sj, true, errors.New("ghsim: simulator JIT config lacks a runner name or token")
	}
	return sj, true, nil
}

func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}
