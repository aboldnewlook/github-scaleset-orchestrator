package scaler

import (
	"context"
	"testing"
	"time"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// remoteScaler builds a scaler whose fleet has one connected remote worker
// and no local member, so every placement is an offer.
func remoteScaler(t *testing.T, gen JITGenerator) (*Scaler, *fleet.Fleet, fleet.WorkerRecord, string, <-chan *workerv1.Directive) {
	t.Helper()
	timing := fleet.DefaultTiming()
	timing.OfferTTL = 80 * time.Millisecond
	timing.Tick = 10 * time.Millisecond
	fl := fleet.New(fleet.Options{Logger: quietTestLogger(), Timing: timing})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fl.Start(ctx)

	w := fleet.WorkerRecord{ID: "id-box", Name: "box", Segment: naming.WorkerSegment("box"),
		Bounds: fleet.Bounds{Classes: []runner.Class{testClass}, MaxSlots: 2}}
	caps := &workerv1.Capabilities{WorkerVersion: "test", Slots: 2, Executors: []*workerv1.Executor{{
		Kind: workerv1.ExecutorKind_EXECUTOR_KIND_DOCKER, Classes: []*workerv1.Class{fleet.ClassToProto(testClass)},
	}}}
	res, err := fl.OpenSession(ctx, w, fleet.OpenSessionParams{
		MachineID: "m", Capabilities: caps, Availability: workerv1.Availability_AVAILABILITY_AVAILABLE,
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := fl.Directives(res.SessionID, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Repo: "org/repo", Class: testClass, ScaleSetID: 42, Client: gen, Placer: fl, Cache: "none", Logger: quietTestLogger()})
	return s, fl, w, res.SessionID, ch
}

func nextOffer(t *testing.T, ch <-chan *workerv1.Directive) *workerv1.Offer {
	t.Helper()
	select {
	case d := <-ch:
		if d.GetOffer() == nil {
			t.Fatalf("directive %v is not an offer", d)
		}
		return d.GetOffer()
	case <-time.After(2 * time.Second):
		t.Fatal("no offer within 2s")
	}
	return nil
}

func TestExpiredOfferIsForgottenAndTheJobReplaced(t *testing.T) {
	gen := &fakeJITConfigGenerator{}
	s, _, _, _, ch := remoteScaler(t, gen)
	if err := s.Scale(context.Background(), statsMessage(1)); err != nil {
		t.Fatal(err)
	}
	first := nextOffer(t, ch)
	if len(s.Runners()) != 1 {
		t.Fatalf("runners = %v while the offer is outstanding, want 1", s.Runners())
	}
	waitPlacement(t, "the unanswered offer to expire and be forgotten", func() bool { return len(s.Runners()) == 0 })
	if gen.Calls() != 0 {
		t.Fatalf("minted %d JIT configs for an offer nobody accepted, want 0", gen.Calls())
	}
	if err := s.Scale(context.Background(), statsMessage(1)); err != nil {
		t.Fatal(err)
	}
	second := nextOffer(t, ch)
	if second.GetOfferId() == first.GetOfferId() || len(s.Runners()) != 1 {
		t.Fatalf("job was not re-placed: offer %q again, runners %v", second.GetOfferId(), s.Runners())
	}
}

func TestLostWorkerSessionFreesTheScalersRunner(t *testing.T) {
	s, fl, w, sid, ch := remoteScaler(t, &fakeJITConfigGenerator{})
	if err := s.Scale(context.Background(), statsMessage(1)); err != nil {
		t.Fatal(err)
	}
	nextOffer(t, ch)
	if err := fl.CloseSession(sid, w.ID); err != nil {
		t.Fatal(err)
	}
	waitPlacement(t, "the closed session's runner to be forgotten", func() bool { return len(s.Runners()) == 0 })
	if got := s.placer.Free(testRequest()); got != 0 {
		t.Fatalf("free = %d with the only worker disconnected, want 0", got)
	}
}
