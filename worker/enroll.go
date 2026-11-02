package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1/workerv1connect"
)

// EnrollOptions are the inputs to Enroll.
type EnrollOptions struct {
	Join      Join
	DialAddr  string // "" = Join.Addr; e.g. "gso-router:9200" on a compose network
	Name      string
	MachineID string
}

// Enroll redeems the join string's single-use code with the router over
// TLS 1.3, trusting only the pinned key, and returns the worker credential
// (store it with a CredentialStore) and the state to persist with SaveState.
// The state keeps the join string's address even when DialAddr overrides it.
func Enroll(ctx context.Context, o EnrollOptions) (string, State, error) {
	addr := o.DialAddr
	if addr == "" {
		addr = o.Join.Addr
	}
	if addr == "" || o.Join.Pin == "" || o.Join.Code == "" {
		return "", State{}, errors.New("enroll: the join string is incomplete")
	}
	resp, err := newRPC(addr, o.Join.Pin).Enroll(ctx, connect.NewRequest(&workerv1.EnrollRequest{
		EnrollmentCode: o.Join.Code,
		WorkerName:     o.Name,
		MachineId:      o.MachineID,
	}))
	if err != nil {
		return "", State{}, fmt.Errorf("enrolling with router %s: %w", addr, err)
	}
	credential := resp.Msg.GetCredential()
	if credential == "" {
		return "", State{}, errors.New("enroll: the router returned no credential")
	}
	b := resp.Msg.GetBounds()
	st := State{
		Name: o.Name,
		Addr: o.Join.Addr,
		Pin:  o.Join.Pin,
		Bounds: Bounds{
			Classes:  classNames(b.GetClasses()),
			MaxSlots: int(b.GetMaxSlots()),
			Repos:    append([]string(nil), b.GetRepos()...),
		},
		OwnerScopes: append([]string(nil), resp.Msg.GetOwnerScopes()...),
		EnrolledAt:  time.Now().UTC(),
	}
	return credential, st, nil
}

// newRPC dials the router over TLS 1.3, trusting only the pinned key. The
// http.Client has no Timeout because Subscribe is a long-lived stream; every
// unary call carries its own context deadline.
func newRPC(addr, pin string) workerv1connect.WorkerServiceClient {
	transport := &http.Transport{
		TLSClientConfig:     PinnedTLSConfig(pin),
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	return workerv1connect.NewWorkerServiceClient(&http.Client{Transport: transport}, "https://"+addr)
}
