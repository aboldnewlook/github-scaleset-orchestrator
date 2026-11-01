package event

import (
	"encoding/json"
	"time"
)

// EventType identifies the kind of event.
type EventType string

const (
	EventRunnerSpawned   EventType = "runner.spawned"
	EventRunnerCompleted EventType = "runner.completed"
	EventRunnerFailed    EventType = "runner.failed"
	EventJobStarted      EventType = "job.started"
	EventJobCompleted    EventType = "job.completed"
	EventScaleSetCreated EventType = "scaleset.created"
	EventScaleSetDeleted EventType = "scaleset.deleted"
	EventScaleDecision   EventType = "scale.decision"
	EventDaemonStarted   EventType = "daemon.started"
	EventDaemonStopping  EventType = "daemon.stopping"
	EventError           EventType = "error"
)

// Event types for remote workers, listener supervision and public-repo policy
// (design spec §5–§8). Payloads never carry JIT configs, worker credentials
// or enrollment codes.
const (
	EventJobAvailable      EventType = "job.available"
	EventJobAssigned       EventType = "job.assigned"
	EventJobReassigned     EventType = "job.reassigned"
	EventListenerDegraded  EventType = "listener.degraded"
	EventListenerRecovered EventType = "listener.recovered"
	EventWorkerEnrolled    EventType = "worker.enrolled"
	EventWorkerConnected   EventType = "worker.connected"
	EventWorkerLost        EventType = "worker.lost"
	EventWorkerRevoked     EventType = "worker.revoked"
	EventOfferRejected     EventType = "offer.rejected"
	EventRunnerAdopted     EventType = "runner.adopted"
	EventRunnerRemoved     EventType = "runner.removed"
	EventAlert             EventType = "alert"
	EventRepoQuarantined   EventType = "repo.quarantined"
	EventForkCancelled     EventType = "fork.cancelled"
)

// Event is a single occurrence in the system.
type Event struct {
	Time    time.Time       `json:"time"`
	Type    EventType       `json:"type"`
	Repo    string          `json:"repo,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
