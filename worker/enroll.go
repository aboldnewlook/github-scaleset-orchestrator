package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
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
	return workerv1connect.NewWorkerServiceClient(&http.Client{Transport: newTransport(pin)}, "https://"+addr)
}

// HTTP/2 liveness pings: a connection whose peer vanished without a FIN or
// RST (laptop sleep, NAT timeout, cable pulled) is detected and closed instead
// of stalling every call. Variables so tests can shorten them.
var (
	pingSendTimeout = 15 * time.Second
	pingTimeout     = 10 * time.Second
)

func newTransport(pin string) *http.Transport {
	return &http.Transport{
		TLSClientConfig:     PinnedTLSConfig(pin),
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		HTTP2:               &http.HTTP2Config{SendPingTimeout: pingSendTimeout, PingTimeout: pingTimeout},
	}
}

// resettableTransport lets the client abandon a wedged connection pool and
// dial afresh: reset swaps in a new transport, and the old one is left to its
// own ping timeouts.
type resettableTransport struct {
	pin string
	mu  sync.Mutex
	t   *http.Transport
}

func newResettableTransport(pin string) *resettableTransport {
	return &resettableTransport{pin: pin, t: newTransport(pin)}
}

func (r *resettableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	t := r.t
	r.mu.Unlock()
	return t.RoundTrip(req)
}

func (r *resettableTransport) reset() {
	r.mu.Lock()
	old := r.t
	r.t = newTransport(r.pin)
	r.mu.Unlock()
	old.CloseIdleConnections()
}
