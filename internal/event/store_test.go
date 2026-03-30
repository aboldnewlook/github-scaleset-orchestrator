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

func TestFileStoreRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Use a tiny max size so rotation triggers quickly.
	store := NewFileStoreWithMaxSize(path, 100)

	now := time.Now().Truncate(time.Millisecond)

	// Write enough events to exceed 100 bytes.
	for i := range 5 {
		e := Event{
			Time: now.Add(time.Duration(i) * time.Second),
			Type: EventRunnerSpawned,
			Repo: "org/repo1",
		}
		if err := store.Append(e); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	// The rotated file should exist.
	rotated := path + ".1"
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf("expected rotated file %s to exist: %v", rotated, err)
	}

	// The current file should exist and be smaller than the rotated file
	// (it has fewer events since rotation just happened).
	currentInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected current file to exist: %v", err)
	}
	if currentInfo.Size() == 0 {
		t.Fatal("current file should not be empty after post-rotation appends")
	}
}

func TestFileStoreQueryAfterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	store := NewFileStoreWithMaxSize(path, 100)

	now := time.Now().Truncate(time.Millisecond)

	// Write events that will cause rotation.
	for i := range 5 {
		e := Event{
			Time: now.Add(time.Duration(i) * time.Second),
			Type: EventRunnerSpawned,
			Repo: "org/repo1",
		}
		if err := store.Append(e); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	// Query should return only events from the current file (post-rotation).
	results, err := store.Query(StoreFilter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	// We should get fewer than 5 events since the older ones are in .1
	if len(results) >= 5 {
		t.Fatalf("expected fewer than 5 events in current file after rotation, got %d", len(results))
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 event in current file after rotation")
	}
}

func TestFileStoreOnlyOneRotatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Very small max size to trigger multiple rotations.
	store := NewFileStoreWithMaxSize(path, 50)

	now := time.Now().Truncate(time.Millisecond)

	// Write many events to trigger multiple rotations.
	for i := range 20 {
		e := Event{
			Time: now.Add(time.Duration(i) * time.Second),
			Type: EventRunnerSpawned,
			Repo: "org/repo1",
		}
		if err := store.Append(e); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	// Only path and path.1 should exist -- no .2, .3, etc.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if name != "events.jsonl" && name != "events.jsonl.1" {
			t.Fatalf("unexpected file in store directory: %s", name)
		}
	}

	// Both files must exist.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("current file missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
}

func TestNewFileStoreWithMaxSizeZeroUsesDefault(t *testing.T) {
	store := NewFileStoreWithMaxSize("/tmp/test.jsonl", 0)
	if store.maxSize != DefaultMaxStoreSize {
		t.Fatalf("expected default max size %d, got %d", DefaultMaxStoreSize, store.maxSize)
	}
}
