package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

func TestRecordStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker", "runners.json")
	store, err := OpenRecordStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.List(); len(got) != 0 {
		t.Fatalf("new store lists %v", got)
	}
	accepted := time.Unix(1_700_000_000, 0).UTC()
	b := RunnerRecord{Name: "gso-mac-prune-bbbbbb", RunnerID: 8, Repo: "jrschumacher/prune", Class: "linux-arm64", Executor: runner.ExecutorDocker, Phase: "starting", AcceptedAt: accepted}
	a := RunnerRecord{Name: "gso-mac-prune-aaaaaa", RunnerID: 7, Repo: "jrschumacher/prune", Class: "linux-arm64", Executor: runner.ExecutorDocker, Phase: "running", AcceptedAt: accepted}
	for _, r := range []RunnerRecord{b, a} {
		if err := store.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := OpenRecordStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.List()
	if len(got) != 2 || got[0].Name != a.Name || got[1].Name != b.Name {
		t.Fatalf("List() = %+v, want a then b (sorted by name)", got)
	}
	if got[0].RunnerID != 7 || got[0].Phase != "running" || got[0].Executor != runner.ExecutorDocker || !got[0].AcceptedAt.Equal(accepted) {
		t.Fatalf("record a did not round-trip: %+v", got[0])
	}
	if err := reopened.Delete(a.Name); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Delete("never-existed"); err != nil {
		t.Fatalf("Delete(missing) = %v, want nil", err)
	}
	again, err := OpenRecordStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.List(); len(got) != 1 || got[0].Name != b.Name {
		t.Fatalf("after Delete: %+v", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("runners.json mode = %04o, want 0600", perm)
		}
	}
}

func TestRecordStoreRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "runners.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRecordStore(corrupt); err == nil {
		t.Fatal("OpenRecordStore accepted a corrupt file; a worker must not forget containers silently")
	}
	store, err := OpenRecordStore(filepath.Join(dir, "fresh.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(RunnerRecord{}); err == nil {
		t.Fatal("Put accepted a record with no name")
	}
}
