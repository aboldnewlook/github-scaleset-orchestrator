package control

import "encoding/json"

// Request is a JSON-RPC-like request sent over the control socket.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	Token  string          `json:"token,omitempty"`
}

// Response is a JSON-RPC-like response returned over the control socket.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Method constants for the control protocol.
const (
	MethodLiveStatus    = "live_status"
	MethodLiveEvents    = "live_events"
	MethodRecycleRunner = "recycle_runner"
	MethodSetMaxRunners = "set_max_runners"
	MethodShutdown      = "shutdown"
)

// LiveEventsParams are the parameters for the live_events method.
type LiveEventsParams struct {
	Since string `json:"since,omitempty"` // RFC3339 timestamp
}

// RecycleRunnerParams are the parameters for the recycle_runner method.
type RecycleRunnerParams struct {
	Name string `json:"name"`
}

// SetMaxRunnersParams are the parameters for the set_max_runners method.
type SetMaxRunnersParams struct {
	Count int `json:"count"`
}

// LiveStatusResult is returned by the live_status method.
type LiveStatusResult struct {
	Repos      []RepoLiveStatus `json:"repos"`
	MaxRunners int              `json:"max_runners"`
	Available  int              `json:"available"`
}

// RepoLiveStatus describes the runners for a single repo.
type RepoLiveStatus struct {
	Repo    string   `json:"repo"`
	Runners []string `json:"runners"`
}
