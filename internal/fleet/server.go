package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1/workerv1connect"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
)

// ServerConfig configures the gso.worker.v1 server.
type ServerConfig struct {
	Registry        *Registry
	Fleet           *Fleet
	Logger          *slog.Logger
	Bus             *event.Bus
	AllowNets       []*net.IPNet            // empty → DefaultAllowNets()
	OwnerScopes     func(b Bounds) []string // seeds the worker allowlist; nil → b.Repos
	EnrollPerMinute int                     // default 5
	EnrollPerHour   int                     // default 30
	Now             func() time.Time
}

// Server implements gso.worker.v1 over the fleet and the registry.
type Server struct {
	c       ServerConfig
	limiter *enrollLimiter
	handler http.Handler

	subMu sync.Mutex
	subs  map[string]*subscription // live Subscribe stream per session
}

// subscription is one live Subscribe stream; a newer stream for the same
// session cancels it and waits for done before reading the directives.
type subscription struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// maxRequestBytes bounds every request body, decompressed, before auth.
const maxRequestBytes = 1 << 20

var _ workerv1connect.WorkerServiceHandler = (*Server)(nil)

// Fixed error messages: Connect errors never echo headers or secrets (spec §4.6).
var (
	errMsgMissingCredential = errors.New("missing worker credential")
	errMsgBadCredential     = errors.New("invalid worker credential")
	errMsgRevoked           = errors.New("worker revoked")
	errMsgBadCode           = errors.New("invalid or expired enrollment code")
	errMsgNameTaken         = errors.New("worker name already enrolled")
	errMsgRateLimited       = errors.New("too many enrollment attempts")
	errMsgSessionActive     = errors.New("session active")
	errMsgUnknownSession    = errors.New("unknown session")
	errMsgOfferNotFound     = errors.New("offer not found")
	errMsgNotPermitted      = errors.New("not permitted")
	errMsgUnavailable       = errors.New("router unavailable")
)

// NewServer builds the handler: Connect service behind the source allowlist.
func NewServer(c ServerConfig) *Server {
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if len(c.AllowNets) == 0 {
		c.AllowNets = DefaultAllowNets()
	}
	if c.EnrollPerMinute <= 0 {
		c.EnrollPerMinute = 5
	}
	if c.EnrollPerHour <= 0 {
		c.EnrollPerHour = 30
	}
	if c.OwnerScopes == nil {
		c.OwnerScopes = func(b Bounds) []string { return append([]string(nil), b.Repos...) }
	}
	s := &Server{c: c, subs: map[string]*subscription{}, limiter: newEnrollLimiter(c.EnrollPerMinute, c.EnrollPerHour, c.Now)}
	mux := http.NewServeMux()
	mux.Handle(workerv1connect.NewWorkerServiceHandler(s, connect.WithReadMaxBytes(maxRequestBytes)))
	s.handler = allowCIDRs(mux, c.AllowNets)
	return s
}

// Handler serves gso.worker.v1; mount it on a TLS 1.3 listener.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) publish(typ event.EventType, payload map[string]any) {
	if s.c.Bus == nil {
		return
	}
	data, _ := json.Marshal(payload)
	s.c.Bus.Publish(event.Event{Time: s.c.Now(), Type: typ, Payload: data})
}

func peerHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func (s *Server) authenticate(h http.Header) (WorkerRecord, error) {
	cred, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if !ok || cred == "" {
		return WorkerRecord{}, connect.NewError(connect.CodeUnauthenticated, errMsgMissingCredential)
	}
	rec, err := s.c.Registry.Authenticate(cred)
	switch {
	case errors.Is(err, ErrRevoked):
		return WorkerRecord{}, connect.NewError(connect.CodePermissionDenied, errMsgRevoked)
	case err != nil:
		return WorkerRecord{}, connect.NewError(connect.CodeUnauthenticated, errMsgBadCredential)
	}
	return rec, nil
}

func (s *Server) fleetError(err error) error {
	switch {
	case errors.Is(err, ErrSessionActive):
		return connect.NewError(connect.CodeFailedPrecondition, errMsgSessionActive)
	case errors.Is(err, ErrUnknownSession):
		return connect.NewError(connect.CodeFailedPrecondition, errMsgUnknownSession)
	case errors.Is(err, ErrOfferNotFound):
		return connect.NewError(connect.CodeNotFound, errMsgOfferNotFound)
	case errors.Is(err, ErrNotPermitted):
		return connect.NewError(connect.CodePermissionDenied, errMsgNotPermitted)
	default:
		s.c.Logger.Error("worker request failed", "error", err)
		return connect.NewError(connect.CodeUnavailable, errMsgUnavailable)
	}
}

// Enroll redeems a one-time code (no credential; rate-limited per source).
func (s *Server) Enroll(_ context.Context, req *connect.Request[workerv1.EnrollRequest]) (*connect.Response[workerv1.EnrollResponse], error) {
	ip := peerHost(req.Peer().Addr)
	if !s.limiter.allow(ip) {
		s.c.Logger.Warn("enrollment rate limit exceeded", "remote", ip)
		return nil, connect.NewError(connect.CodeResourceExhausted, errMsgRateLimited)
	}
	cred, rec, err := s.c.Registry.Enroll(req.Msg.GetEnrollmentCode(), req.Msg.GetWorkerName(), req.Msg.GetMachineId(), s.c.Now())
	switch {
	case errors.Is(err, ErrNameTaken):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errMsgNameTaken)
	case errors.Is(err, ErrInvalidCode):
		s.c.Logger.Warn("enrollment refused", "remote", ip)
		return nil, connect.NewError(connect.CodeUnauthenticated, errMsgBadCode)
	case err != nil:
		s.c.Logger.Error("enrollment failed", "remote", ip, "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, errMsgUnavailable)
	}
	if requested := req.Msg.GetWorkerName(); requested != "" && requested != rec.Name {
		s.c.Logger.Info("worker enrolled under the admin-declared name", "requested", requested, "worker", rec.Name)
	}
	s.c.Logger.Info("worker enrolled", "worker", rec.Name, "id", rec.ID, "remote", ip)
	s.publish(event.EventWorkerEnrolled, map[string]any{"worker": rec.Name, "id": rec.ID})
	return connect.NewResponse(&workerv1.EnrollResponse{
		Credential: cred, Bounds: boundsToProto(rec.Bounds), OwnerScopes: s.c.OwnerScopes(rec.Bounds),
	}), nil
}

// OpenSession clamps capabilities, reconciles running runners and rejects a
// second live session.
func (s *Server) OpenSession(ctx context.Context, req *connect.Request[workerv1.OpenSessionRequest]) (*connect.Response[workerv1.OpenSessionResponse], error) {
	rec, err := s.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	// A long running[] is not refused: a worker whose slots were lowered while
	// runners still run must be able to reconnect. Fleet.OpenSession asks
	// GitHub about at most the free slots' worth of plausible candidates and
	// returns every other entry in RejectedRunners.
	res, err := s.c.Fleet.OpenSession(ctx, rec, OpenSessionParams{
		MachineID: req.Msg.GetMachineId(), Capabilities: req.Msg.GetCapabilities(), Availability: req.Msg.GetAvailability(),
		Running: req.Msg.GetRunning(), AcceptPublic: req.Msg.GetAcceptPublic(),
	})
	if err != nil {
		if errors.Is(err, ErrSessionActive) {
			s.c.Logger.Warn("second session rejected while one is live", "worker", rec.Name, "remote", peerHost(req.Peer().Addr))
		}
		return nil, s.fleetError(err)
	}
	return connect.NewResponse(&workerv1.OpenSessionResponse{
		SessionId: res.SessionID, Effective: res.Effective, Policy: res.Policy,
		HeartbeatInterval: durationpb.New(res.HeartbeatInterval), OfferTtl: durationpb.New(res.OfferTTL),
		RejectedRunners: res.RejectedRunners,
	}), nil
}

func keepaliveDirective() *workerv1.Directive {
	return &workerv1.Directive{Kind: &workerv1.Directive_Keepalive{Keepalive: &workerv1.Keepalive{}}}
}

// Subscribe streams the session's directives, with a Keepalive first and
// every Timing.Keepalive, until the session ends or the client goes away.
func (s *Server) Subscribe(ctx context.Context, req *connect.Request[workerv1.SubscribeRequest], stream *connect.ServerStream[workerv1.Directive]) error {
	rec, err := s.authenticate(req.Header())
	if err != nil {
		return err
	}
	ch, err := s.c.Fleet.Directives(req.Msg.GetSessionId(), rec.ID)
	if err != nil {
		return s.fleetError(err)
	}
	// One live stream per session: a newer Subscribe supersedes the older one
	// (which ends cleanly) so a stale half-open stream cannot keep draining
	// directives from the shared channel.
	sid := req.Msg.GetSessionId()
	ctx, cancel := context.WithCancel(ctx)
	sub := &subscription{cancel: cancel, done: make(chan struct{})}
	s.subMu.Lock()
	old := s.subs[sid]
	s.subs[sid] = sub
	s.subMu.Unlock()
	defer func() {
		close(sub.done)
		s.subMu.Lock()
		if s.subs[sid] == sub {
			delete(s.subs, sid)
		}
		s.subMu.Unlock()
		cancel()
	}()
	if old != nil {
		old.cancel()
		wait := time.NewTimer(s.c.Fleet.o.Timing.Keepalive)
		defer wait.Stop()
		select {
		case <-old.done:
		case <-wait.C: // the old handler is stuck writing to a dead connection; it is cancelled and will not read again
		case <-ctx.Done():
			return nil
		}
	}
	if err := stream.Send(keepaliveDirective()); err != nil {
		return err
	}
	ticker := time.NewTicker(s.c.Fleet.o.Timing.Keepalive)
	defer ticker.Stop()
	for {
		// A superseded or finished handler must not take another directive:
		// check before every receive (select alone picks among ready cases at
		// random).
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-ch:
			if !ok {
				return nil // session ended; the worker reopens one
			}
			if err := stream.Send(d); err != nil {
				return err
			}
		case <-ticker.C:
			if err := stream.Send(keepaliveDirective()); err != nil {
				return err
			}
		}
	}
}

// Heartbeat records liveness and returns the public policy.
func (s *Server) Heartbeat(ctx context.Context, req *connect.Request[workerv1.HeartbeatRequest]) (*connect.Response[workerv1.HeartbeatResponse], error) {
	rec, err := s.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	policy, err := s.c.Fleet.Heartbeat(ctx, req.Msg.GetSessionId(), rec.ID, req.Msg.GetAvailability(), req.Msg.GetRunning(), req.Msg.GetAcceptPublic())
	if err != nil {
		return nil, s.fleetError(err)
	}
	return connect.NewResponse(&workerv1.HeartbeatResponse{Policy: policy}), nil
}

// AcceptOffer mints and returns the JIT config. The response is never logged.
func (s *Server) AcceptOffer(ctx context.Context, req *connect.Request[workerv1.AcceptOfferRequest]) (*connect.Response[workerv1.AcceptOfferResponse], error) {
	rec, err := s.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	resp, err := s.c.Fleet.AcceptOffer(ctx, req.Msg.GetSessionId(), rec.ID, req.Msg.GetOfferId())
	if err != nil {
		return nil, s.fleetError(err)
	}
	s.c.Logger.Info("offer accepted", "worker", rec.Name, "runner", resp.GetRunnerName(), "runner_id", resp.GetRunnerId())
	return connect.NewResponse(resp), nil
}

// RejectOffer ends an offer, removing the runner if it was already minted.
func (s *Server) RejectOffer(ctx context.Context, req *connect.Request[workerv1.RejectOfferRequest]) (*connect.Response[workerv1.RejectOfferResponse], error) {
	rec, err := s.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	if err := s.c.Fleet.RejectOffer(ctx, req.Msg.GetSessionId(), rec.ID, req.Msg.GetOfferId(), req.Msg.GetReason()); err != nil {
		return nil, s.fleetError(err)
	}
	return connect.NewResponse(&workerv1.RejectOfferResponse{}), nil
}

// ReportRunner records a runner phase for a runner placed on this worker.
func (s *Server) ReportRunner(ctx context.Context, req *connect.Request[workerv1.ReportRunnerRequest]) (*connect.Response[workerv1.ReportRunnerResponse], error) {
	rec, err := s.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	if err := s.c.Fleet.ReportRunner(ctx, req.Msg.GetSessionId(), rec.ID, req.Msg); err != nil {
		return nil, s.fleetError(err)
	}
	s.c.Logger.Debug("runner reported", "worker", rec.Name, "runner", req.Msg.GetRunnerName(), "phase", req.Msg.GetPhase().String())
	return connect.NewResponse(&workerv1.ReportRunnerResponse{}), nil
}

// CloseSession ends the session cleanly (the worker is quitting).
func (s *Server) CloseSession(_ context.Context, req *connect.Request[workerv1.CloseSessionRequest]) (*connect.Response[workerv1.CloseSessionResponse], error) {
	rec, err := s.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	if err := s.c.Fleet.CloseSession(req.Msg.GetSessionId(), rec.ID); err != nil {
		return nil, s.fleetError(err)
	}
	s.c.Logger.Info("worker session closed", "worker", rec.Name, "reason", req.Msg.GetReason())
	return connect.NewResponse(&workerv1.CloseSessionResponse{}), nil
}
