package event

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreAppendAndQuery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	store := NewFileStore(path)

	now := time.Now().Truncate(time.Millisecond)

	events := []Event{
		{Time: now.Add(-2 * time.Minute), Type: EventRunnerSpawned, Repo: "org/repo1"},
		{Time: now.Add(-1 * time.Minute), Type: EventRunnerFailed, Repo: "org/repo2"},
		{Time: now, Type: EventJobCompleted, Repo: "org/repo1"},
	}

	for _, e := range events {
		if err := store.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	// Query all
	all, err := store.Query(StoreFilter{})
	if err != nil {
		t.Fatalf("Query all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d events, want 3", len(all))
	}

	// Query by type
	byType, err := store.Query(StoreFilter{Type: EventRunnerFailed})
	if err != nil {
		t.Fatalf("Query by type: %v", err)
	}
	if len(byType) != 1 || byType[0].Repo != "org/repo2" {
		t.Fatalf("unexpected result filtering by type: %+v", byType)
	}

	// Query by repo
	byRepo, err := store.Query(StoreFilter{Repo: "org/repo1"})
	if err != nil {
		t.Fatalf("Query by repo: %v", err)
	}
	if len(byRepo) != 2 {
		t.Fatalf("got %d events for repo1, want 2", len(byRepo))
	}

	// Query by since
	bySince, err := store.Query(StoreFilter{Since: now.Add(-90 * time.Second)})
	if err != nil {
		t.Fatalf("Query by since: %v", err)
	}
	if len(bySince) != 2 {
		t.Fatalf("got %d events since cutoff, want 2", len(bySince))
	}
}

func TestFileStoreQueryMissingFile(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "nonexistent.jsonl"))

	results, err := store.Query(StoreFilter{})
	if err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty results, got %d", len(results))
	}
}

func TestFileStoreWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	store := NewFileStore(path)

	e := Event{
		Time:    time.Now().Truncate(time.Millisecond),
		Type:    EventDaemonStarted,
		Payload: json.RawMessage(`{"key":"value"}`),
	}
	if err := store.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Should be a single line ending with newline
	if data[len(data)-1] != '\n' {
		t.Fatal("expected trailing newline")
	}

	var decoded Event
	if err := json.Unmarshal(data[:len(data)-1], &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Type != EventDaemonStarted {
		t.Fatalf("got type %q, want %q", decoded.Type, EventDaemonStarted)
	}
}
