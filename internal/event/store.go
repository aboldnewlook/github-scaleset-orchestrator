package event

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// StoreFilter controls which events are returned by Query.
type StoreFilter struct {
	Since time.Time
	Type  EventType
	Repo  string
}

// FileStore is an append-only JSONL file store for events.
type FileStore struct {
	mu   sync.Mutex
	path string
}

// NewFileStore creates a new FileStore that writes to path. The file is
// created on first Append if it does not exist.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// Append writes a single event as one JSON line.
func (s *FileStore) Append(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = f.Write(data)
	return err
}

// Query reads back events that match filter. An empty filter returns all
// events.
func (s *FileStore) Query(filter StoreFilter) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var results []Event
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // skip malformed lines
		}
		if !filter.Since.IsZero() && e.Time.Before(filter.Since) {
			continue
		}
		if filter.Type != "" && e.Type != filter.Type {
			continue
		}
		if filter.Repo != "" && e.Repo != filter.Repo {
			continue
		}
		results = append(results, e)
	}
	return results, scanner.Err()
}
