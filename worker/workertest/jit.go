package workertest

import (
	"encoding/base64"
	"encoding/json"
)

const defaultServerURL = "https://pipelines.actions.githubusercontent.com/workertest"

// FakeJIT returns a JIT config in the actions/runner format: base64 of a JSON
// object that maps runner file names to their base64 contents, where the
// ".runner" file carries gitHubUrl. worker.VerifyJIT reads exactly this shape,
// so tests can mint a config for any repository without GitHub.
func FakeJIT(gitHubURL, runnerName string) string {
	return fakeJIT(gitHubURL, defaultServerURL, runnerName)
}

func fakeJIT(gitHubURL, serverURL, runnerName string) string {
	settings, _ := json.Marshal(struct {
		AgentName  string `json:"agentName"`
		GitHubURL  string `json:"gitHubUrl"`
		ServerURL  string `json:"serverUrl"`
		WorkFolder string `json:"workFolder"`
	}{runnerName, gitHubURL, serverURL, "_work"})
	files, _ := json.Marshal(map[string]string{
		".runner":      base64.StdEncoding.EncodeToString(settings),
		".credentials": base64.StdEncoding.EncodeToString([]byte("{}")),
	})
	return base64.StdEncoding.EncodeToString(files)
}
