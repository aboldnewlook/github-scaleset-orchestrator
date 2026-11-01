package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Bounds are the limits the admin set at enrollment (classes as "linux-x64").
type Bounds struct {
	Classes  []string `json:"classes"`
	MaxSlots int      `json:"max_slots"`
	Repos    []string `json:"repos,omitempty"`
}

// State is the enrolled worker's worker.json: everything but the credential,
// which lives in a CredentialStore.
type State struct {
	Name        string    `json:"name"`
	Addr        string    `json:"addr"` // the advertise address from the join string
	Pin         string    `json:"pin"`
	Bounds      Bounds    `json:"bounds"`
	OwnerScopes []string  `json:"owner_scopes"`
	EnrolledAt  time.Time `json:"enrolled_at"`
}

// LoadState reads worker.json. A missing file returns an error matching
// os.ErrNotExist: the worker has not enrolled.
func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("reading worker state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parsing worker state %s: %w", path, err)
	}
	return s, nil
}

// SaveState writes worker.json atomically with mode 0600.
func SaveState(path string, s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing worker state: %w", err)
	}
	return nil
}
