package fleet

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1/workerv1connect"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type serverEnv struct {
	reg    *Registry
	fleet  *Fleet
	client workerv1connect.WorkerServiceClient
	hc     *http.Client
	url    string
	logs   *syncBuffer
	events <-chan event.Event
}

func newServerEnv(t *testing.T, mutate ...func(*ServerConfig)) *serverEnv {
	t.Helper()
	reg, err := OpenRegistry(filepath.Join(t.TempDir(), "workers.json"))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := event.NewBus(nil)
	_, events := bus.Subscribe(1024)
	tm := DefaultTiming()
	tm.Keepalive = 20 * time.Millisecond
	fl := New(Options{Logger: logger, Bus: bus, Timing: tm, GitHub: newFakeGitHub(), Adopter: &fakeAdopter{}})
	cfg := ServerConfig{Registry: reg, Fleet: fl, Logger: logger, Bus: bus}
	for _, m := range mutate {
		m(&cfg)
	}
	tlsCfg, pin, err := LoadOrCreateTLS(filepath.Join(t.TempDir(), "tls"), logger)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(NewServer(cfg).Handler())
	srv.EnableHTTP2 = true
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: worker.PinnedTLSConfig(pin), ForceAttemptHTTP2: true}}
	t.Cleanup(hc.CloseIdleConnections)
	return &serverEnv{reg: reg, fleet: fl, client: workerv1connect.NewWorkerServiceClient(hc, srv.URL), hc: hc, url: srv.URL, logs: logs, events: events}
}

func authed[T any](msg *T, credential string) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+credential)
	return req
}

func (e *serverEnv) enroll(t *testing.T, name string, b Bounds) (string, WorkerRecord) {
	t.Helper()
	code, _, err := e.reg.AddEnrollment(name, b, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Enroll(context.Background(), connect.NewRequest(&workerv1.EnrollRequest{EnrollmentCode: code, WorkerName: name, MachineId: "machine-" + name}))
	if err != nil {
		t.Fatalf("Enroll(%s): %v", name, err)
	}
	rec, ok := e.reg.ByName(name)
	if !ok {
		t.Fatalf("%s not in the registry", name)
	}
	return resp.Msg.GetCredential(), rec
}

func (e *serverEnv) openSession(t *testing.T, cred string, caps *workerv1.Capabilities) *workerv1.OpenSessionResponse {
	t.Helper()
	resp, err := e.client.OpenSession(context.Background(), authed(&workerv1.OpenSessionRequest{
		MachineId: "m", Capabilities: caps, Availability: workerv1.Availability_AVAILABILITY_AVAILABLE,
	}, cred))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	return resp.Msg
}

func (e *serverEnv) sawEvent(typ event.EventType) bool {
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-e.events:
			if ev.Type == typ {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func oneSlot(c runner.Class) Bounds { return Bounds{Classes: []runner.Class{c}, MaxSlots: 1} }

func TestEnrollReturnsCredentialBoundsAndScopes(t *testing.T) {
	e := newServerEnv(t)
	code, _, err := e.reg.AddEnrollment("mac-m1", Bounds{Classes: []runner.Class{linuxArm64}, MaxSlots: 2, Repos: []string{"o/a"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Enroll(context.Background(), connect.NewRequest(&workerv1.EnrollRequest{EnrollmentCode: code, WorkerName: "laptop", MachineId: "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}\.[A-Za-z0-9_-]{43}$`).MatchString(resp.Msg.GetCredential()) {
		t.Fatalf("credential = %q", resp.Msg.GetCredential())
	}
	b := resp.Msg.GetBounds()
	if b.GetMaxSlots() != 2 || len(b.GetClasses()) != 1 || len(b.GetRepos()) != 1 || b.GetRepos()[0] != "o/a" {
		t.Fatalf("bounds = %v", b)
	}
	if scopes := resp.Msg.GetOwnerScopes(); len(scopes) != 1 || scopes[0] != "o/a" {
		t.Fatalf("owner scopes = %v", scopes)
	}
	if !e.sawEvent(event.EventWorkerEnrolled) {
		t.Fatal("no worker.enrolled event")
	}
	if _, err := e.client.Enroll(context.Background(), connect.NewRequest(&workerv1.EnrollRequest{EnrollmentCode: code})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("reused code = %v", err)
	}

	scoped := newServerEnv(t, func(c *ServerConfig) {
		c.OwnerScopes = func(Bounds) []string { return []string{"jrschumacher/*"} }
	})
	code, _, _ = scoped.reg.AddEnrollment("box", oneSlot(linuxX64), time.Now())
	resp, err = scoped.client.Enroll(context.Background(), connect.NewRequest(&workerv1.EnrollRequest{EnrollmentCode: code}))
	if err != nil || len(resp.Msg.GetOwnerScopes()) != 1 || resp.Msg.GetOwnerScopes()[0] != "jrschumacher/*" {
		t.Fatalf("custom scopes = %v, %v", resp, err)
	}
}

func TestAuthMatrix(t *testing.T) {
	e := newServerEnv(t)
	cred, _ := e.enroll(t, "box", oneSlot(linuxX64))
	id, _, _ := strings.Cut(cred, ".")
	revoked, _ := e.enroll(t, "gone", oneSlot(linuxX64))
	if _, err := e.reg.Revoke("gone"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, header string
		want         connect.Code
	}{
		{"missing", "", connect.CodeUnauthenticated},
		{"not bearer", "Basic " + cred, connect.CodeUnauthenticated},
		{"empty bearer", "Bearer ", connect.CodeUnauthenticated},
		{"wrong secret", "Bearer " + id + ".wrong", connect.CodeUnauthenticated},
		{"unknown worker", "Bearer ffffffffffffffff.abc", connect.CodeUnauthenticated},
		{"revoked", "Bearer " + revoked, connect.CodePermissionDenied},
	}
	for _, tt := range tests {
		req := connect.NewRequest(&workerv1.OpenSessionRequest{Capabilities: dockerCaps(1, linuxX64)})
		if tt.header != "" {
			req.Header().Set("Authorization", tt.header)
		}
		_, err := e.client.OpenSession(context.Background(), req)
		if connect.CodeOf(err) != tt.want {
			t.Errorf("%s: code = %v, want %v (%v)", tt.name, connect.CodeOf(err), tt.want, err)
		}
	}
	e.openSession(t, cred, dockerCaps(1, linuxX64)) // the valid credential works
}

func TestEnrollRateLimit(t *testing.T) {
	clock := newTestClock()
	e := newServerEnv(t, func(c *ServerConfig) { c.Now = clock.Now })
	bad := func() error {
		_, err := e.client.Enroll(context.Background(), connect.NewRequest(&workerv1.EnrollRequest{EnrollmentCode: "bad"}))
		return err
	}
	for i := 1; i <= 5; i++ {
		if err := bad(); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if err := bad(); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("6th attempt: %v, want ResourceExhausted", err)
	}
	code, _, err := e.reg.AddEnrollment("box", oneSlot(linuxX64), clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	good := connect.NewRequest(&workerv1.EnrollRequest{EnrollmentCode: code})
	if _, err := e.client.Enroll(context.Background(), good); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("a valid code bypassed the limit: %v", err)
	}
	clock.Advance(time.Minute)
	if _, err := e.client.Enroll(context.Background(), connect.NewRequest(&workerv1.EnrollRequest{EnrollmentCode: code})); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestSecondSessionIsFailedPrecondition(t *testing.T) {
	e := newServerEnv(t)
	cred, _ := e.enroll(t, "box", oneSlot(linuxX64))
	e.openSession(t, cred, dockerCaps(1, linuxX64))
	_, err := e.client.OpenSession(context.Background(), authed(&workerv1.OpenSessionRequest{Capabilities: dockerCaps(1, linuxX64)}, cred))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "session active") {
		t.Fatalf("second session = %v", err)
	}
}

func TestSubscribeStreamsKeepalivesAndDirectives(t *testing.T) {
	e := newServerEnv(t)
	cred, rec := e.enroll(t, "box", oneSlot(linuxX64))
	open := e.openSession(t, cred, dockerCaps(1, linuxX64))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := e.client.Subscribe(ctx, authed(&workerv1.SubscribeRequest{SessionId: open.GetSessionId()}, cred))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	keepalives, drained := 0, false
	for !drained && stream.Receive() {
		d := stream.Msg()
		switch {
		case d.GetKeepalive() != nil:
			keepalives++
			if keepalives == 2 {
				if err := e.fleet.DrainWorker(rec.ID, "maintenance"); err != nil {
					t.Fatal(err)
				}
			}
		case d.GetDrain() != nil:
			drained = d.GetDrain().GetReason() == "maintenance"
		}
	}
	if !drained {
		t.Fatalf("stream ended without the drain directive: %v", stream.Err())
	}

	bad, err := e.client.Subscribe(ctx, authed(&workerv1.SubscribeRequest{SessionId: "nope"}, cred))
	if err != nil {
		t.Fatal(err)
	}
	for bad.Receive() {
	}
	if connect.CodeOf(bad.Err()) != connect.CodeFailedPrecondition {
		t.Fatalf("unknown session stream = %v", bad.Err())
	}
}

func TestOfferRoundTripOverConnect(t *testing.T) {
	e := newServerEnv(t)
	cred, _ := e.enroll(t, "box", oneSlot(linuxX64))
	otherCred, _ := e.enroll(t, "other", oneSlot(linuxX64))
	open := e.openSession(t, cred, dockerCaps(1, linuxX64))
	otherOpen := e.openSession(t, otherCred, dockerCaps(1, linuxX64))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := e.client.Subscribe(ctx, authed(&workerv1.SubscribeRequest{SessionId: open.GetSessionId()}, cred))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	r, ok := e.fleet.Reserve(PlacementRequest{Repo: "o/r", Class: linuxX64})
	if !ok || r.Member() != "box" {
		t.Fatalf("Reserve = %v, %v", r, ok)
	}
	errc := runAsync(ctx, r, (&mintRecorder{}).mint)
	var offer *workerv1.Offer
	for offer == nil && stream.Receive() {
		offer = stream.Msg().GetOffer()
	}
	if offer == nil {
		t.Fatalf("no offer: %v", stream.Err())
	}
	accept := func(sid, credential string) (*connect.Response[workerv1.AcceptOfferResponse], error) {
		return e.client.AcceptOffer(ctx, authed(&workerv1.AcceptOfferRequest{SessionId: sid, OfferId: offer.GetOfferId()}, credential))
	}
	if _, err := accept(otherOpen.GetSessionId(), otherCred); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("another worker's accept = %v", err)
	}
	if _, err := accept(open.GetSessionId(), otherCred); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("someone else's session = %v", err)
	}
	resp, err := accept(open.GetSessionId(), cred)
	if err != nil || resp.Msg.GetRunnerName() != r.RunnerName() || resp.Msg.GetEncodedJitConfig() != "jit-"+r.RunnerName() {
		t.Fatalf("accept = %v, %v", resp, err)
	}
	if _, err := accept(open.GetSessionId(), cred); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("second accept = %v", err)
	}
	for _, phase := range []workerv1.RunnerPhase{workerv1.RunnerPhase_RUNNER_PHASE_RUNNING, workerv1.RunnerPhase_RUNNER_PHASE_EXITED} {
		if _, err := e.client.ReportRunner(ctx, authed(&workerv1.ReportRunnerRequest{SessionId: open.GetSessionId(), RunnerName: r.RunnerName(), Phase: phase}, cred)); err != nil {
			t.Fatalf("ReportRunner(%v): %v", phase, err)
		}
	}
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Run = %v", err)
	}
	hb, err := e.client.Heartbeat(ctx, authed(&workerv1.HeartbeatRequest{SessionId: open.GetSessionId(), Availability: workerv1.Availability_AVAILABILITY_AVAILABLE}, cred))
	if err != nil || hb.Msg.GetPolicy() == nil {
		t.Fatalf("Heartbeat = %v, %v", hb, err)
	}
	if _, err := e.client.CloseSession(ctx, authed(&workerv1.CloseSessionRequest{SessionId: open.GetSessionId(), Reason: "quit"}, cred)); err != nil {
		t.Fatal(err)
	}
	_, err = e.client.Heartbeat(ctx, authed(&workerv1.HeartbeatRequest{SessionId: open.GetSessionId()}, cred))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("heartbeat after close = %v", err)
	}
}

func TestSourcesOutsideTheAllowlistAreRefused(t *testing.T) {
	_, testNet, _ := net.ParseCIDR("192.0.2.0/24")
	e := newServerEnv(t, func(c *ServerConfig) { c.AllowNets = []*net.IPNet{testNet} })
	resp, err := e.hc.Post(e.url+workerv1connect.WorkerServiceEnrollProcedure, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}
