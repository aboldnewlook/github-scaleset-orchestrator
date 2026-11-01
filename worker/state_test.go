package worker

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker", "worker.json")
	if _, err := LoadState(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadState(missing) = %v, want os.ErrNotExist", err)
	}
	want := State{
		Name:        "mac-m1",
		Addr:        "unraid.lan:9200",
		Pin:         testPin,
		Bounds:      Bounds{Classes: []string{"linux-arm64"}, MaxSlots: 4, Repos: []string{"jrschumacher/prune"}},
		OwnerScopes: []string{"jrschumacher/*"},
		EnrolledAt:  time.Unix(1_700_000_000, 0).UTC(),
	}
	if err := SaveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != want.Name || got.Addr != want.Addr || got.Pin != want.Pin || got.Bounds.MaxSlots != 4 ||
		len(got.Bounds.Classes) != 1 || got.Bounds.Classes[0] != "linux-arm64" || len(got.Bounds.Repos) != 1 ||
		len(got.OwnerScopes) != 1 || got.OwnerScopes[0] != "jrschumacher/*" || !got.EnrolledAt.Equal(want.EnrolledAt) {
		t.Fatalf("LoadState() = %+v, want %+v", got, want)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("worker.json mode = %04o, want 0600", perm)
		}
	}
}
