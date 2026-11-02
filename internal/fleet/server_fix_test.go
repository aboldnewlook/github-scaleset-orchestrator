package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
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

func TestOpenSessionRefusesMoreRunningThanSlots(t *testing.T) {
	e := newServerEnv(t)
	cred, _ := e.enroll(t, "box", oneSlot(linuxX64)) // MaxSlots 1
	var running []*workerv1.RunnerState
	for i := 0; i < 2; i++ {
		running = append(running, runningState("gso-box-r-abcde"+string(rune('a'+i)), "o/r", linuxX64, workerv1.RunnerPhase_RUNNER_PHASE_RUNNING))
	}
	_, err := e.client.OpenSession(context.Background(), authed(&workerv1.OpenSessionRequest{Capabilities: dockerCaps(1, linuxX64), Running: running}, cred))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("2 running on a 1-slot worker = %v, want InvalidArgument", err)
	}
	if strings.Contains(err.Error(), "gso-box-r") {
		t.Fatalf("error echoes request data: %v", err)
	}
	if _, err := e.client.OpenSession(context.Background(), authed(&workerv1.OpenSessionRequest{Capabilities: dockerCaps(1, linuxX64), Running: running[:1]}, cred)); err != nil {
		t.Fatalf("1 running on a 1-slot worker = %v", err)
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
