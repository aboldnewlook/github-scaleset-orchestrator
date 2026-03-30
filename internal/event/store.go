package event

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// DefaultMaxStoreSize is the default maximum file size before rotation (10 MB).
const DefaultMaxStoreSize int64 = 10 * 1024 * 1024

// StoreFilter controls which events are returned by Query.
type StoreFilter struct {
	Since time.Time
	Type  EventType
	Repo  string
}

// FileStore is an append-only JSONL file store for events with size-based
// rotation. When the current file exceeds maxSize, it is renamed to
// <path>.1 and a fresh file is created. At most one rotated file is kept.
type FileStore struct {
	mu      sync.Mutex
	path    string
	maxSize int64
}

// NewFileStore creates a new FileStore that writes to path. The file is
// created on first Append if it does not exist. Log rotation occurs when
// the file exceeds DefaultMaxStoreSize.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path, maxSize: DefaultMaxStoreSize}
}

// NewFileStoreWithMaxSize creates a new FileStore with a custom maximum file
// size in bytes. When the file exceeds this size, it is rotated.
func NewFileStoreWithMaxSize(path string, maxSize int64) *FileStore {
	if maxSize <= 0 {
		maxSize = DefaultMaxStoreSize
	}
	return &FileStore{path: path, maxSize: maxSize}
}

// Append writes a single event as one JSON line. If the current file
// exceeds the configured maximum size, it is rotated before writing.
func (s *FileStore) Append(e Event) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.rotateIfNeeded(); err != nil {
		return err
	}

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = f.Write(data)
	return err
}

// rotateIfNeeded checks the current file size and rotates if it exceeds
// maxSize. Must be called with s.mu held.
func (s *FileStore) rotateIfNeeded() error {
	info, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if info.Size() <= s.maxSize {
		return nil
	}

	rotated := s.path + ".1"
	return os.Rename(s.path, rotated)
}

// Query reads back events that match filter. An empty filter returns all
// events. Only the current (non-rotated) file is searched.
func (s *FileStore) Query(filter StoreFilter) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.queryFile(s.path, filter)
}

// queryFile reads events from a single file that match the filter.
func (s *FileStore) queryFile(path string, filter StoreFilter) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

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
