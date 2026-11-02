package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
)

type mockWorkerAdmin struct {
	*mockOrchState
	added   []control.WorkersAddParams
	updated []control.WorkersUpdateParams
	drained []string
	revoked []string
	list    []control.WorkerLiveStatus
}

func (m *mockWorkerAdmin) AddWorker(_ context.Context, p control.WorkersAddParams) (control.WorkersAddResult, error) {
	m.added = append(m.added, p)
	return control.WorkersAddResult{Join: "gso-join:v1:router.lan:9200:" + strings.Repeat("a", 64) + ":" + strings.Repeat("b", 32), ExpiresAt: time.Unix(1700000000, 0).UTC()}, nil
}

func (m *mockWorkerAdmin) UpdateWorker(_ context.Context, p control.WorkersUpdateParams) error {
	m.updated = append(m.updated, p)
	return nil
}
func (m *mockWorkerAdmin) ListWorkers() []control.WorkerLiveStatus { return m.list }
func (m *mockWorkerAdmin) DrainWorker(name, _ string) error {
	m.drained = append(m.drained, name)
	return nil
}

func (m *mockWorkerAdmin) RevokeWorker(_ context.Context, name string) error {
	m.revoked = append(m.revoked, name)
	return nil
}

var _ service.WorkerAdmin = (*mockWorkerAdmin)(nil)

func workersRequest(t *testing.T, method string, params any) control.Request {
	t.Helper()
	req := control.Request{Method: method}
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		req.Params = data
	}
	return req
}

func TestWorkersMethodsRequireTheUnixSocket(t *testing.T) {
	admin := &mockWorkerAdmin{mockOrchState: &mockOrchState{maxRunners: 1}}
	rt := service.NewRuntime(admin, func() {}, testLogger())
	for _, method := range []string{control.MethodWorkersAdd, control.MethodWorkersUpdate, control.MethodWorkersList, control.MethodWorkersDrain, control.MethodWorkersRevoke} {
		for name, ctx := range map[string]context.Context{
			"no transport": context.Background(),
			"tcp":          control.WithTransport(context.Background(), false),
		} {
			resp := rt.HandleRequest(ctx, workersRequest(t, method, control.WorkerNameParams{Name: "w1"}))
			if resp.Error != "workers_* methods are only available on the local unix socket" {
				t.Errorf("%s over %s: error = %q", method, name, resp.Error)
			}
		}
	}
	if len(admin.added)+len(admin.updated)+len(admin.drained)+len(admin.revoked) != 0 {
		t.Fatal("a refused request reached the worker admin")
	}
}

func TestWorkersAddOverTheUnixSocket(t *testing.T) {
	admin := &mockWorkerAdmin{mockOrchState: &mockOrchState{maxRunners: 1}}
	rt := service.NewRuntime(admin, func() {}, testLogger())
	ctx := control.WithTransport(context.Background(), true)
	resp := rt.HandleRequest(ctx, workersRequest(t, control.MethodWorkersAdd, control.WorkersAddParams{Name: "mac", Classes: []string{"linux-arm64"}, Slots: 4}))
	if resp.Error != "" {
		t.Fatalf("workers_add error: %s", resp.Error)
	}
	var res control.WorkersAddResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Join, "gso-join:v1:") || res.ExpiresAt.IsZero() {
		t.Fatalf("result = %+v", res)
	}
	if len(admin.added) != 1 || admin.added[0].Name != "mac" || admin.added[0].Slots != 4 {
		t.Fatalf("admin saw %+v", admin.added)
	}
	for _, tc := range []struct {
		method string
		params any
		check  func() bool
	}{
		{control.MethodWorkersUpdate, control.WorkersUpdateParams{Name: "mac", Classes: []string{"linux-arm64"}, Slots: 2}, func() bool { return len(admin.updated) == 1 }},
		{control.MethodWorkersDrain, control.WorkerNameParams{Name: "mac", Reason: "update"}, func() bool { return len(admin.drained) == 1 }},
		{control.MethodWorkersRevoke, control.WorkerNameParams{Name: "mac"}, func() bool { return len(admin.revoked) == 1 }},
	} {
		if resp := rt.HandleRequest(ctx, workersRequest(t, tc.method, tc.params)); resp.Error != "" || !tc.check() {
			t.Fatalf("%s: error %q", tc.method, resp.Error)
		}
	}
}

func TestWorkersMethodsNeedAWorkerAdmin(t *testing.T) {
	rt := service.NewRuntime(&mockOrchState{maxRunners: 1}, func() {}, testLogger())
	resp := rt.HandleRequest(control.WithTransport(context.Background(), true), workersRequest(t, control.MethodWorkersList, nil))
	if resp.Error != "this daemon does not manage workers" {
		t.Fatalf("error = %q", resp.Error)
	}
}

func TestLiveStatusIncludesWorkers(t *testing.T) {
	admin := &mockWorkerAdmin{
		mockOrchState: &mockOrchState{maxRunners: 1},
		list:          []control.WorkerLiveStatus{{Name: "mac", ID: "0123456789abcdef", Connected: true, Availability: "available", Slots: 4, Classes: []string{"linux-arm64"}}},
	}
	rt := service.NewRuntime(admin, func() {}, testLogger())
	status, err := rt.LiveStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Workers) != 1 || status.Workers[0].Name != "mac" {
		t.Fatalf("workers = %+v", status.Workers)
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var back control.LiveStatusResult
	if err := json.Unmarshal(data, &back); err != nil || len(back.Workers) != 1 || back.Workers[0].Slots != 4 {
		t.Fatalf("round trip = %+v, %v", back.Workers, err)
	}
}
