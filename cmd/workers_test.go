package cmd

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
)

func TestWorkersCommandsRefuseRemote(t *testing.T) {
	saved := remoteAddr
	t.Cleanup(func() { remoteAddr = saved })
	remoteAddr = "router.lan:9100"
	if _, err := workersClient(); err == nil || !strings.Contains(err.Error(), "unix socket") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		worker  string
		classes []string
		slots   int
		wantErr string
	}{
		{"ok", "mac", []string{"linux-arm64"}, 4, ""},
		{"no name", "", []string{"linux-x64"}, 1, "worker name"},
		{"no classes", "mac", nil, 1, "--classes"},
		{"zero slots", "mac", []string{"linux-x64"}, 0, "--slots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkerBounds(tc.worker, tc.classes, tc.slots)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestFormatWorkerRows(t *testing.T) {
	seen := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	rows := formatWorkerRows([]control.WorkerLiveStatus{
		{Name: "local", Local: true, Connected: true, Availability: "available", Slots: 2, Busy: 1, Classes: []string{"linux-x64"}},
		{Name: "mac", ID: "0123456789abcdef", Connected: true, Availability: "draining", Slots: 4, Busy: 2, Classes: []string{"linux-arm64"}, AcceptPublic: true, LastSeen: seen},
		{Name: "old", ID: "fedcba9876543210", Availability: "offline", Slots: 1, Revoked: true},
		{Name: "pc", ID: "00000000000000aa", Availability: "lost", Slots: 2},
	})
	want := [][]string{
		{"local (local)", "-", "available", "1/2", "linux-x64", "no", "-"},
		{"mac", "0123456789abcdef", "draining", "2/4", "linux-arm64", "yes", seen.Local().Format(time.RFC3339)},
		{"old", "fedcba9876543210", "revoked", "0/1", "-", "no", "-"},
		{"pc", "00000000000000aa", "offline", "0/2", "-", "no", "-"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows =\n%v\nwant\n%v", rows, want)
	}
}
