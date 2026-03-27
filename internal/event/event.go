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

// Event is a single occurrence in the system.
type Event struct {
	Time    time.Time       `json:"time"`
	Type    EventType       `json:"type"`
	Repo    string          `json:"repo,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
