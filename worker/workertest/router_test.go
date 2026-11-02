package workertest_test

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker/workertest"
)

func TestFakeJITParsesAsTheRunnerFormat(t *testing.T) {
	info, err := worker.ParseJIT(workertest.FakeJIT("https://github.com/acme/app", "gso-test-abc123"))
	if err != nil {
		t.Fatalf("ParseJIT(FakeJIT) error: %v", err)
	}
	if info.Host != "github.com" || info.Repo != "acme/app" {
		t.Fatalf("ParseJIT(FakeJIT) = %+v, want host github.com and repo acme/app", info)
	}
}

func TestRouterJoinCarriesItsAddressAndPin(t *testing.T) {
	r := workertest.NewRouter(t)
	j, err := worker.ParseJoin(r.Join("box"))
	if err != nil {
		t.Fatal(err)
	}
	if j.Addr != r.Addr() || j.Pin != r.Pin() || len(j.Code) != 32 {
		t.Fatalf("join = %+v, want addr %s, pin %s and a 32-char code", j, r.Addr(), r.Pin())
	}
}

func TestRouterRefusesTLSBelow13(t *testing.T) {
	r := workertest.NewRouter(t)
	conn, err := tls.Dial("tcp", r.Addr(), &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}) //nolint:gosec // probing the server's version floor
	if err == nil {
		_ = conn.Close()
		t.Fatal("router accepted a TLS 1.2 handshake")
	}
}

func TestRouterCountsOnlyOpenedSessions(t *testing.T) {
	r := workertest.NewRouter(t)
	j, err := worker.ParseJoin(r.Join("box"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := worker.Enroll(ctx, worker.EnrollOptions{Join: j, Name: "box", MachineID: "m"}); err != nil {
		t.Fatalf("Enroll() error: %v", err)
	}
	if got := r.Sessions(); got != 0 {
		t.Fatalf("Sessions() = %d before any OpenSession, want 0", got)
	}
}
