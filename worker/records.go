package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// RunnerRecord is what a worker persists for each runner it launched, so it can
// re-attach after a restart instead of killing the job (design spec §7).
type RunnerRecord struct {
	Name       string              `json:"name"`
	RunnerID   int64               `json:"runner_id"`
	Repo       string              `json:"repo"`
	Class      string              `json:"class"`
	Executor   runner.ExecutorKind `json:"executor"`
	Phase      string              `json:"phase"` // "starting" | "running"
	AcceptedAt time.Time           `json:"accepted_at"`
}

// RecordStore is the worker's runners.json: one record per live runner, written
// atomically with mode 0600 on every change.
type RecordStore struct {
	mu      sync.Mutex
	path    string
	records map[string]RunnerRecord
}

// OpenRecordStore loads path, or starts empty if it does not exist. A corrupt
// file is an error: silently forgetting records would make the worker remove
// containers that are still running jobs.
func OpenRecordStore(path string) (*RecordStore, error) {
	s := &RecordStore{path: path, records: make(map[string]RunnerRecord)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading runner records: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return s, nil
	}
	var list []RunnerRecord
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parsing runner records %s: %w", path, err)
	}
	for _, r := range list {
		s.records[r.Name] = r
	}
	return s, nil
}

// List returns the records sorted by runner name.
func (s *RecordStore) List() []RunnerRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedLocked()
}

// Put adds or replaces a record and persists the store.
func (s *RecordStore) Put(r RunnerRecord) error {
	if r.Name == "" {
		return errors.New("runner record has no name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.records[r.Name]
	s.records[r.Name] = r
	if err := s.persistLocked(); err != nil {
		if had {
			s.records[r.Name] = prev
		} else {
			delete(s.records, r.Name)
		}
		return err
	}
	return nil
}

// Delete removes a record (a missing one is not an error) and persists.
func (s *RecordStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.records[name]
	if !had {
		return nil
	}
	delete(s.records, name)
	if err := s.persistLocked(); err != nil {
		s.records[name] = prev
		return err
	}
	return nil
}

func (s *RecordStore) sortedLocked() []RunnerRecord {
	list := make([]RunnerRecord, 0, len(s.records))
	for _, r := range s.records {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

func (s *RecordStore) persistLocked() error {
	data, err := json.MarshalIndent(s.sortedLocked(), "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing runner records: %w", err)
	}
	return nil
}
