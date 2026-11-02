package fleet

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1/workerv1connect"
)

// clockedFleet returns a ServerConfig mutator that swaps in a fleet driven by
// clock, with a fast keepalive, and stores it in *out.
func clockedFleet(clock *testClock, out **Fleet) func(*ServerConfig) {
	return func(c *ServerConfig) {
		tm := DefaultTiming()
		tm.Keepalive = 20 * time.Millisecond
		*out = New(Options{Logger: c.Logger, Bus: c.Bus, Timing: tm, GitHub: newFakeGitHub(), Adopter: &fakeAdopter{}, Now: clock.Now})
		c.Fleet = *out
	}
}

// expectStreamEnds drains the stream and asserts the contract worker.Client
// relies on: a clean end (Receive false, Err nil), within a short deadline.
func expectStreamEnds(t *testing.T, name string, stream *connect.ServerStreamForClient[workerv1.Directive]) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for stream.Receive() {
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: stream did not end", name)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("%s: stream ended with %v, want a clean end", name, err)
	}
}

func TestSubscribeEndsCleanlyWhenTheSessionEnds(t *testing.T) {
	ends := map[string]func(t *testing.T, e *serverEnv, fl *Fleet, clock *testClock, rec WorkerRecord, cred, sid string){
		"CloseSession": func(t *testing.T, e *serverEnv, _ *Fleet, _ *testClock, _ WorkerRecord, cred, sid string) {
			if _, err := e.client.CloseSession(context.Background(), authed(&workerv1.CloseSessionRequest{SessionId: sid}, cred)); err != nil {
				t.Fatal(err)
			}
		},
		"replacement OpenSession": func(t *testing.T, e *serverEnv, _ *Fleet, clock *testClock, _ WorkerRecord, cred, _ string) {
			clock.Advance(DefaultTiming().Lost + time.Second)
			e.openSession(t, cred, dockerCaps(1, linuxX64))
		},
		"lost sweep": func(t *testing.T, _ *serverEnv, fl *Fleet, clock *testClock, _ WorkerRecord, _, _ string) {
			clock.Advance(DefaultTiming().Lost + time.Second)
			fl.sweep(context.Background())
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			clock := newTestClock()
			var fl *Fleet
			e := newServerEnv(t, func(c *ServerConfig) { c.Now = clock.Now }, clockedFleet(clock, &fl))
			cred, rec := e.enroll(t, "box", oneSlot(linuxX64))
			sid := e.openSession(t, cred, dockerCaps(1, linuxX64)).GetSessionId()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream, err := e.client.Subscribe(ctx, authed(&workerv1.SubscribeRequest{SessionId: sid}, cred))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			if !stream.Receive() { // the first keepalive: the stream is live
				t.Fatalf("no first message: %v", stream.Err())
			}
			end(t, e, fl, clock, rec, cred, sid)
			expectStreamEnds(t, name, stream)

			again, err := e.client.Subscribe(ctx, authed(&workerv1.SubscribeRequest{SessionId: sid}, cred))
			if err != nil {
				t.Fatal(err)
			}
			for again.Receive() {
			}
			if connect.CodeOf(again.Err()) != connect.CodeFailedPrecondition {
				t.Fatalf("re-subscribe = %v, want FailedPrecondition", again.Err())
			}
		})
	}
}

func TestServerSubscribeSurvivesAPartialFleetTiming(t *testing.T) {
	e := newServerEnv(t, func(c *ServerConfig) {
		c.Fleet = New(Options{Logger: c.Logger, Timing: Timing{Tick: time.Millisecond}, GitHub: newFakeGitHub(), Adopter: &fakeAdopter{}})
	})
	cred, _ := e.enroll(t, "box", oneSlot(linuxX64))
	// Opening a session uses the replaced fleet through the server.
	sid := e.openSession(t, cred, dockerCaps(1, linuxX64)).GetSessionId()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := e.client.Subscribe(ctx, authed(&workerv1.SubscribeRequest{SessionId: sid}, cred))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Receive() || stream.Msg().GetKeepalive() == nil {
		t.Fatalf("no keepalive: %v", stream.Err())
	}
}

func TestOversizedRequestsAreRefusedBeforeAuth(t *testing.T) {
	e := newServerEnv(t)
	big := strings.Repeat("a", 2<<20)
	_, err := e.client.Enroll(context.Background(), connect.NewRequest(&workerv1.EnrollRequest{EnrollmentCode: "x", WorkerName: big}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("oversized Enroll = %v, want ResourceExhausted", err)
	}
	_, err = e.client.OpenSession(context.Background(), connect.NewRequest(&workerv1.OpenSessionRequest{MachineId: big}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("oversized unauthenticated OpenSession = %v, want ResourceExhausted", err)
	}
}

// A worker whose slots were lowered while runners still run must still be
// able to reconnect: the session opens, GitHub is asked about at most the
// bound, and every unexamined entry comes back in RejectedRunners.
func TestOpenSessionWithMoreRunningThanSlotsStillOpens(t *testing.T) {
	gh := newFakeGitHub()
	gh.setScaleSet("o/r", linuxX64, 7)
	e := newServerEnv(t, func(c *ServerConfig) {
		tm := DefaultTiming()
		c.Fleet = New(Options{Logger: c.Logger, Bus: c.Bus, Timing: tm, GitHub: gh, Adopter: &fakeAdopter{}})
	})
	cred, _ := e.enroll(t, "box", oneSlot(linuxX64)) // MaxSlots 1
	var running []*workerv1.RunnerState
	var names []string
	for i := 0; i < 20; i++ {
		n := "gso-box-r-abcd" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		names = append(names, n)
		running = append(running, runningState(n, "o/r", linuxX64, workerv1.RunnerPhase_RUNNER_PHASE_RUNNING))
	}
	resp, err := e.client.OpenSession(context.Background(), authed(&workerv1.OpenSessionRequest{Capabilities: dockerCaps(1, linuxX64), Running: running}, cred))
	if err != nil {
		t.Fatalf("20 running on a 1-slot worker = %v, want a session", err)
	}
	if resp.Msg.GetSessionId() == "" {
		t.Fatal("no session id")
	}
	if n := gh.getCalls(); n > 1 {
		t.Fatalf("GetRunnerByName called %d times for a 1-slot worker, want at most 1", n)
	}
	if got := len(resp.Msg.GetRejectedRunners()); got != len(names) {
		t.Fatalf("rejected %d of %d (none are known to GitHub)", got, len(names))
	}
}

func TestANewSubscribeSupersedesTheOldOne(t *testing.T) {
	e := newServerEnv(t)
	cred, _ := e.enroll(t, "box", oneSlot(linuxX64))
	sid := e.openSession(t, cred, dockerCaps(1, linuxX64)).GetSessionId()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := e.client.Subscribe(ctx, authed(&workerv1.SubscribeRequest{SessionId: sid}, cred))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if !first.Receive() {
		t.Fatalf("first stream: %v", first.Err())
	}
	second, err := e.client.Subscribe(ctx, authed(&workerv1.SubscribeRequest{SessionId: sid}, cred))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	offers := make(chan struct{}, 4)
	go func() {
		for first.Receive() {
			if first.Msg().GetOffer() != nil {
				offers <- struct{}{}
			}
		}
		close(offers)
	}()
	select {
	case _, open := <-offers:
		if open {
			t.Fatal("the superseded stream received an offer")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the superseded stream did not end")
	}
	if err := first.Err(); err != nil {
		t.Fatalf("superseded stream ended with %v, want a clean end", err)
	}

	r, ok := e.fleet.Reserve(PlacementRequest{Repo: "o/r", Class: linuxX64})
	if !ok {
		t.Fatal("Reserve failed")
	}
	errc := runAsync(ctx, r, (&mintRecorder{}).mint)
	var offer *workerv1.Offer
	for offer == nil && second.Receive() {
		offer = second.Msg().GetOffer()
	}
	if offer == nil {
		t.Fatalf("the current stream got no offer: %v", second.Err())
	}
	r.Release()
	_ = waitErr(t, errc)
}

// A superseded handler stuck writing to a half-open connection (the client
// never reads) must not delay the new stream beyond about one keepalive, and
// must not take directives meant for the new stream.
func TestSupersedingAStuckStreamIsBoundedByTheKeepalive(t *testing.T) {
	e := newServerEnv(t)
	cred, rec := e.enroll(t, "box", oneSlot(linuxX64))
	sid := e.openSession(t, cred, dockerCaps(1, linuxX64)).GetSessionId()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stuckCtx, stuckCancel := context.WithCancel(ctx)
	defer stuckCancel()
	// A client with tiny HTTP/2 receive windows that never reads: the
	// server's writes block once the windows and socket buffers fill.
	slowTransport := e.hc.Transport.(*http.Transport).Clone()
	slowTransport.HTTP2 = &http.HTTP2Config{MaxReceiveBufferPerStream: 64 << 10, MaxReceiveBufferPerConnection: 64 << 10}
	slowHC := &http.Client{Transport: slowTransport}
	defer slowHC.CloseIdleConnections()
	slowClient := workerv1connect.NewWorkerServiceClient(slowHC, e.url)
	stuck, err := slowClient.Subscribe(stuckCtx, authed(&workerv1.SubscribeRequest{SessionId: sid}, cred))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stuck.Close() }()
	// Never read from stuck. Flood it with large directives until the
	// handler's writes block on flow control and the channel backs up.
	big := strings.Repeat("x", 1<<20)
	queued := 0
	for i := 0; i < directiveBuffer; i++ {
		e.fleet.mu.Lock()
		m := e.fleet.members[rec.ID]
		ok := e.fleet.sendLocked(m, cancelDirective("gso-box-r-aaaaaa", big))
		e.fleet.mu.Unlock()
		if !ok {
			break
		}
		queued++
		time.Sleep(2 * time.Millisecond)
	}
	if queued < 20 {
		t.Fatalf("only queued %d directives", queued)
	}

	start := time.Now()
	second, err := e.client.Subscribe(ctx, authed(&workerv1.SubscribeRequest{SessionId: sid}, cred))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if !second.Receive() {
		t.Fatalf("new stream never went live: %v", second.Err())
	}
	t.Logf("queued=%d live after %v", queued, time.Since(start))
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("new stream took %v to go live", d)
	}
	r, ok := e.fleet.Reserve(PlacementRequest{Repo: "o/r", Class: linuxX64})
	if !ok {
		t.Fatal("Reserve failed")
	}
	errc := runAsync(ctx, r, (&mintRecorder{}).mint)
	var offer *workerv1.Offer
	for offer == nil && second.Receive() {
		offer = second.Msg().GetOffer()
	}
	if offer == nil {
		t.Fatalf("the new stream got no offer: %v", second.Err())
	}
	r.Release()
	_ = waitErr(t, errc)
}
