package control

import (
	"encoding/json"
	"time"
)

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
	Repos      []RepoLiveStatus   `json:"repos"`
	MaxRunners int                `json:"max_runners"`
	Available  int                `json:"available"`
	Workers    []WorkerLiveStatus `json:"workers,omitempty"`
}

// RepoLiveStatus describes the runners for a single repo.
type RepoLiveStatus struct {
	Repo    string   `json:"repo"`
	Runners []string `json:"runners"`
}

// Worker administration methods. They are served only on the local unix
// socket (spec §8.1, SEC-F8).
const (
	MethodWorkersAdd    = "workers_add"
	MethodWorkersUpdate = "workers_update"
	MethodWorkersList   = "workers_list"
	MethodWorkersDrain  = "workers_drain"
	MethodWorkersRevoke = "workers_revoke"
)

// WorkersAddParams are the parameters for workers_add.
type WorkersAddParams struct {
	Name      string   `json:"name"`
	Classes   []string `json:"classes"`
	Slots     int      `json:"slots"`
	Repos     []string `json:"repos,omitempty"`
	Advertise string   `json:"advertise,omitempty"`
}

// WorkersAddResult carries the one-time join string. It contains the
// enrollment code: never log it.
type WorkersAddResult struct {
	Join      string    `json:"join"`
	ExpiresAt time.Time `json:"expires_at"`
}

// WorkersUpdateParams are the parameters for workers_update.
type WorkersUpdateParams struct {
	Name    string   `json:"name"`
	Classes []string `json:"classes"`
	Slots   int      `json:"slots"`
	Repos   []string `json:"repos,omitempty"`
}

// WorkerNameParams name one worker (workers_drain, workers_revoke).
type WorkerNameParams struct {
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"`
}

// WorkerLiveStatus describes one fleet member or enrolled worker.
type WorkerLiveStatus struct {
	Name         string    `json:"name"`
	ID           string    `json:"id"`
	Local        bool      `json:"local"`
	Connected    bool      `json:"connected"`
	Availability string    `json:"availability"`
	Slots        int       `json:"slots"`
	Busy         int       `json:"busy"`
	Classes      []string  `json:"classes"`
	AcceptPublic bool      `json:"accept_public"`
	LastSeen     time.Time `json:"last_seen,omitempty"`
	Revoked      bool      `json:"revoked,omitempty"`
}
