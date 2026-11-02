package worker_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker/workertest"
)

func enrollCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestEnrollReturnsCredentialAndState(t *testing.T) {
	r := workertest.NewRouter(t)
	j, err := worker.ParseJoin(r.Join("box"))
	if err != nil {
		t.Fatal(err)
	}
	cred, st, err := worker.Enroll(enrollCtx(t), worker.EnrollOptions{Join: j, Name: "box", MachineID: "machine-1"})
	if err != nil {
		t.Fatalf("Enroll() error: %v", err)
	}
	if cred == "" || !strings.Contains(cred, ".") {
		t.Fatalf("credential = %q, want <worker_id>.<secret>", cred)
	}
	if st.Name != "box" || st.Addr != r.Addr() || st.Pin != r.Pin() || st.EnrolledAt.IsZero() {
		t.Fatalf("state = %+v", st)
	}
	if !slices.Equal(st.OwnerScopes, []string{"acme/*"}) {
		t.Fatalf("owner scopes = %v, want [acme/*]", st.OwnerScopes)
	}
	if st.Bounds.MaxSlots != 4 || !slices.Equal(st.Bounds.Classes, []string{"linux-x64", "linux-arm64"}) {
		t.Fatalf("bounds = %+v", st.Bounds)
	}
}

func TestEnrollCodeIsSingleUse(t *testing.T) {
	r := workertest.NewRouter(t)
	j, err := worker.ParseJoin(r.Join("box"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := worker.Enroll(enrollCtx(t), worker.EnrollOptions{Join: j, Name: "box"}); err != nil {
		t.Fatalf("first Enroll() error: %v", err)
	}
	_, _, err = worker.Enroll(enrollCtx(t), worker.EnrollOptions{Join: j, Name: "box"})
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("second Enroll() error = %v, want Unauthenticated", err)
	}
}

func TestEnrollRefusesARouterWithTheWrongKey(t *testing.T) {
	r := workertest.NewRouter(t)
	j, err := worker.ParseJoin(r.Join("box"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := j
	wrong.Pin = strings.Repeat("0", 64)
	if _, _, err := worker.Enroll(enrollCtx(t), worker.EnrollOptions{Join: wrong, Name: "box"}); err == nil {
		t.Fatal("Enroll() succeeded although the router's key does not match the pin")
	}
	// The handshake failed before the code was sent, so the code still works.
	if _, _, err := worker.Enroll(enrollCtx(t), worker.EnrollOptions{Join: j, Name: "box"}); err != nil {
		t.Fatalf("Enroll() with the right pin error: %v", err)
	}
}

func TestEnrollDialAddrOverridesTheJoinAddress(t *testing.T) {
	r := workertest.NewRouter(t)
	j, err := worker.ParseJoin(r.Join("box"))
	if err != nil {
		t.Fatal(err)
	}
	j.Addr = "router.invalid:9200" // what a containerised router advertises
	_, st, err := worker.Enroll(enrollCtx(t), worker.EnrollOptions{Join: j, DialAddr: r.Addr(), Name: "box"})
	if err != nil {
		t.Fatalf("Enroll() via DialAddr error: %v", err)
	}
	if st.Addr != "router.invalid:9200" {
		t.Fatalf("state addr = %q, want the join string's address", st.Addr)
	}
}
