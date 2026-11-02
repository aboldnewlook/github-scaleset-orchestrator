package fleet

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
)

// Spec §8.3: the JIT config, the bearer credential and the enrollment code
// never appear in slog output, events, or Connect error messages.
func TestSecretsNeverReachLogsEventsOrErrors(t *testing.T) {
	e := newServerEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	marker, err := randomHex(8)
	if err != nil {
		t.Fatal(err)
	}
	jitSecret := "JIT-REDACT-" + marker
	code, _, err := e.reg.AddEnrollment("box", oneSlot(linuxX64), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := e.client.Enroll(ctx, connect.NewRequest(&workerv1.EnrollRequest{EnrollmentCode: code, WorkerName: "box", MachineId: "m"}))
	if err != nil {
		t.Fatal(err)
	}
	cred := enrolled.Msg.GetCredential()
	_, secret, _ := strings.Cut(cred, ".")

	var errs []string
	collect := func(err error) {
		if err == nil {
			t.Fatal("expected this call to be refused")
		}
		errs = append(errs, err.Error())
	}
	_, err = e.client.Enroll(ctx, connect.NewRequest(&workerv1.EnrollRequest{EnrollmentCode: code})) // reused code
	collect(err)
	_, err = e.client.OpenSession(ctx, authed(&workerv1.OpenSessionRequest{}, cred+"x")) // wrong secret
	collect(err)
	_, err = e.client.OpenSession(ctx, authed(&workerv1.OpenSessionRequest{}, "ffffffffffffffff."+secret)) // secret under another ID
	collect(err)

	open := e.openSession(t, cred, dockerCaps(1, linuxX64))
	_, err = e.client.OpenSession(ctx, authed(&workerv1.OpenSessionRequest{Capabilities: dockerCaps(1, linuxX64)}, cred)) // session active
	collect(err)
	stream, err := e.client.Subscribe(ctx, authed(&workerv1.SubscribeRequest{SessionId: open.GetSessionId()}, cred))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	r, ok := e.fleet.Reserve(PlacementRequest{Repo: "o/r", Class: linuxX64})
	if !ok {
		t.Fatal("Reserve failed")
	}
	errc := runAsync(ctx, r, func(context.Context, string) (int64, string, error) { return 77, jitSecret, nil })
	var offer *workerv1.Offer
	for offer == nil && stream.Receive() {
		offer = stream.Msg().GetOffer()
	}
	if offer == nil {
		t.Fatalf("no offer: %v", stream.Err())
	}
	accepted, err := e.client.AcceptOffer(ctx, authed(&workerv1.AcceptOfferRequest{SessionId: open.GetSessionId(), OfferId: offer.GetOfferId()}, cred))
	if err != nil || accepted.Msg.GetEncodedJitConfig() != jitSecret {
		t.Fatalf("the JIT config was not delivered: %v", err)
	}
	_, err = e.client.AcceptOffer(ctx, authed(&workerv1.AcceptOfferRequest{SessionId: open.GetSessionId(), OfferId: offer.GetOfferId()}, cred)) // already accepted
	collect(err)
	if _, err := e.client.ReportRunner(ctx, authed(&workerv1.ReportRunnerRequest{SessionId: open.GetSessionId(), RunnerName: r.RunnerName(), Phase: workerv1.RunnerPhase_RUNNER_PHASE_EXITED}, cred)); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Run = %v", err)
	}

	var events []string
	for drained := false; !drained; {
		select {
		case ev := <-e.events:
			data, _ := json.Marshal(ev)
			events = append(events, string(data))
		default:
			drained = true
		}
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "worker enrolled") || !strings.Contains(logs, "offer accepted") {
		t.Fatal("log capture is not wired; the assertions below would prove nothing")
	}
	haystacks := map[string]string{"logs": logs, "events": strings.Join(events, "\n"), "errors": strings.Join(errs, "\n")}
	secrets := map[string]string{"enrollment code": code, "credential secret": secret, "JIT config": jitSecret}
	for where, text := range haystacks {
		for what, value := range secrets {
			if strings.Contains(text, value) {
				t.Errorf("the %s leaked into %s", what, where)
			}
		}
	}
}
