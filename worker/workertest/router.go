// Package workertest provides an in-process fake gso router for testing
// worker clients without a real router or GitHub. gso's worker tests and the
// Burst desktop app's end-to-end test both use it.
package workertest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1/workerv1connect"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

const (
	heartbeatInterval = 50 * time.Millisecond
	keepaliveInterval = 75 * time.Millisecond
	offerTTL          = 2 * time.Second
	maxSlots          = 4
	waitForSession    = 5 * time.Second
)

// Every worker enrolled through a Router gets these bounds and owner scopes.
var (
	enrolledClasses = []runner.Class{{OS: "linux", Arch: "x64"}, {OS: "linux", Arch: "arm64"}}
	ownerScopes     = []string{"acme/*"}
)

type session struct {
	id         string
	worker     string
	directives chan *workerv1.Directive
	done       chan struct{}
}

type offerEntry struct {
	offer   *workerv1.Offer
	session string
}

// Router is a minimal fake of the gso router's worker service, served over
// TLS 1.3 on 127.0.0.1 with its own key. It keeps one live session (a new
// OpenSession replaces the old one), echoes the advertised capabilities as the
// effective ones (slots capped at 4), uses a 50 ms heartbeat, and mints
// FakeJIT configs for the offered repository.
type Router struct {
	t      testing.TB
	srv    *httptest.Server
	pin    string
	closed chan struct{}

	mu              sync.Mutex
	codes           map[string]string // enrollment code → worker name
	creds           map[string]string // credential → worker name
	revoked         map[string]bool
	defaultAccept   bool
	jitRepoOverride string
	jitHostOverride string
	failOpen        int
	rejectOnOpen    []string
	sess            *session
	sessions        int
	lastOpen        *workerv1.OpenSessionRequest
	lastHeartbeat   *workerv1.HeartbeatRequest
	offers          map[string]offerEntry
	offerRunner     map[string]string // offer ID → runner name, kept after accept
	accepted        []string
	rejected        []string
	reports         []*workerv1.ReportRunnerRequest
	nextRunnerID    int64
}

var _ workerv1connect.WorkerServiceHandler = (*Router)(nil)

// NewRouter starts a Router; t.Cleanup stops it.
func NewRouter(t testing.TB) *Router {
	t.Helper()
	cert, err := selfSigned()
	if err != nil {
		t.Fatalf("workertest: %v", err)
	}
	r := &Router{
		t:           t,
		pin:         worker.SPKIPin(cert.Leaf),
		closed:      make(chan struct{}),
		codes:       make(map[string]string),
		creds:       make(map[string]string),
		revoked:     make(map[string]bool),
		offers:      make(map[string]offerEntry),
		offerRunner: make(map[string]string),
	}
	path, handler := workerv1connect.NewWorkerServiceHandler(r)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	r.srv = httptest.NewUnstartedServer(mux)
	r.srv.EnableHTTP2 = true
	r.srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	r.srv.StartTLS()
	t.Cleanup(r.close)
	return r
}

func (r *Router) close() {
	r.mu.Lock()
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	if r.sess != nil {
		close(r.sess.done)
		r.sess = nil
	}
	r.mu.Unlock()
	r.srv.CloseClientConnections()
	r.srv.Close()
}

// Join returns a join string with a fresh single-use enrollment code.
func (r *Router) Join(name string) string {
	code := randomHex(16)
	r.mu.Lock()
	r.codes[code] = name
	r.mu.Unlock()
	return worker.FormatJoin(r.Addr(), r.pin, code)
}

// Addr is the router's host:port.
func (r *Router) Addr() string { return r.srv.Listener.Addr().String() }

// Pin is the SPKI pin of the router's key.
func (r *Router) Pin() string { return r.pin }

// SetWorkerDefaultAccept sets the pushed PublicPolicy.worker_default_accept.
func (r *Router) SetWorkerDefaultAccept(b bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultAccept = b
}

// SetJITRepoOverride makes AcceptOffer mint JIT configs for repo instead of
// the offered repository, to exercise worker-side JIT verification.
func (r *Router) SetJITRepoOverride(repo string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jitRepoOverride = repo
}

// SetJITHostOverride makes AcceptOffer mint JIT configs whose serverUrl points
// at baseURL (for example "http://127.0.0.1:9300") while gitHubUrl stays
// https://github.com/<repo>, as a simulator router would. "" restores the
// GitHub URLs.
func (r *Router) SetJITHostOverride(baseURL string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jitHostOverride = strings.TrimRight(baseURL, "/")
}

// FailNextOpenSessions makes the next n OpenSession calls fail with
// FailedPrecondition("session active"), as a router does while a crashed
// worker's previous session has not yet expired.
func (r *Router) FailNextOpenSessions(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOpen = n
}

// SetRejectedRunners lists runner names the next OpenSession response returns
// in rejected_runners (failed adoption after a router restart).
func (r *Router) SetRejectedRunners(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejectOnOpen = append([]string(nil), names...)
}

// Revoke makes every call with that worker's credential fail with PermissionDenied.
func (r *Router) Revoke(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked[name] = true
	if r.sess != nil && r.sess.worker == name {
		close(r.sess.done)
		r.sess = nil
	}
}

// Offer sends an offer for repo and class to the live session, waiting up to
// 5 s for one. The offer expires after 2 s.
func (r *Router) Offer(repo, class string, public bool) (offerID, runnerName string) {
	r.t.Helper()
	return r.OfferNamed(repo, class, public, "gso-test-"+randomHex(3))
}

// OfferNamed is Offer with a caller-chosen runner name, for tests of how a
// worker treats a hostile or malformed name.
func (r *Router) OfferNamed(repo, class string, public bool, runnerName string) (offerID, name string) {
	r.t.Helper()
	cl, err := runner.ParseClass(class)
	if err != nil {
		r.t.Fatalf("workertest: %v", err)
	}
	s := r.liveSession()
	offerID = randomHex(16)
	o := &workerv1.Offer{
		OfferId:    offerID,
		RunnerName: runnerName,
		Repo:       repo,
		Class:      classToProto(cl),
		Executor:   executorToProto(cl.Executor()),
		PublicRepo: public,
		ExpiresAt:  timestamppb.New(time.Now().Add(offerTTL)),
		CacheMode:  "none",
	}
	r.mu.Lock()
	r.offers[offerID] = offerEntry{offer: o, session: s.id}
	r.offerRunner[offerID] = runnerName
	r.mu.Unlock()
	r.push(s, &workerv1.Directive{Kind: &workerv1.Directive_Offer{Offer: o}})
	return offerID, runnerName
}

// SendCancel sends a Cancel directive for runnerName to the live session.
func (r *Router) SendCancel(runnerName, reason string) {
	r.t.Helper()
	r.push(r.liveSession(), &workerv1.Directive{Kind: &workerv1.Directive_Cancel{Cancel: &workerv1.Cancel{RunnerName: runnerName, Reason: reason}}})
}

// SendDrain sends a Drain directive to the live session.
func (r *Router) SendDrain(reason string) {
	r.t.Helper()
	r.push(r.liveSession(), &workerv1.Directive{Kind: &workerv1.Directive_Drain{Drain: &workerv1.Drain{Reason: reason}}})
}

// WaitPhase fails t unless runnerName reports phase within timeout.
func (r *Router) WaitPhase(t testing.TB, runnerName string, phase workerv1.RunnerPhase, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		found := slices.ContainsFunc(r.reports, func(rep *workerv1.ReportRunnerRequest) bool {
			return rep.GetRunnerName() == runnerName && rep.GetPhase() == phase
		})
		r.mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workertest: runner %s did not report %s within %s", runnerName, phase, timeout)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Accepted lists the runner names of accepted offers.
func (r *Router) Accepted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.accepted)
}

// Rejected lists the runner names of rejected offers (before or after accept).
func (r *Router) Rejected() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.rejected)
}

// Sessions counts successful OpenSession calls.
func (r *Router) Sessions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions
}

// Reports returns every ReportRunner request received, oldest first.
func (r *Router) Reports() []*workerv1.ReportRunnerRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*workerv1.ReportRunnerRequest, len(r.reports))
	for i, rep := range r.reports {
		out[i] = proto.Clone(rep).(*workerv1.ReportRunnerRequest)
	}
	return out
}

// LastOpenSession returns the latest successful OpenSession request, or nil.
func (r *Router) LastOpenSession() *workerv1.OpenSessionRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastOpen == nil {
		return nil
	}
	return proto.Clone(r.lastOpen).(*workerv1.OpenSessionRequest)
}

// LastHeartbeat returns the latest heartbeat, or nil.
func (r *Router) LastHeartbeat() *workerv1.HeartbeatRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastHeartbeat == nil {
		return nil
	}
	return proto.Clone(r.lastHeartbeat).(*workerv1.HeartbeatRequest)
}

func (r *Router) liveSession() *session {
	r.t.Helper()
	deadline := time.Now().Add(waitForSession)
	for {
		r.mu.Lock()
		s := r.sess
		r.mu.Unlock()
		if s != nil {
			return s
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("workertest: no worker session within %s", waitForSession)
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *Router) push(s *session, d *workerv1.Directive) {
	r.t.Helper()
	select {
	case s.directives <- d:
	case <-time.After(waitForSession):
		r.t.Fatalf("workertest: directive queue is full")
	}
}

// --- WorkerServiceHandler ---

func (r *Router) Enroll(_ context.Context, req *connect.Request[workerv1.EnrollRequest]) (*connect.Response[workerv1.EnrollResponse], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name, ok := r.codes[req.Msg.GetEnrollmentCode()]
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or expired enrollment code"))
	}
	delete(r.codes, req.Msg.GetEnrollmentCode())
	credential := randomHex(8) + "." + randomHex(32)
	r.creds[credential] = name
	classes := make([]*workerv1.Class, 0, len(enrolledClasses))
	for _, c := range enrolledClasses {
		classes = append(classes, classToProto(c))
	}
	return connect.NewResponse(&workerv1.EnrollResponse{
		Credential:  credential,
		Bounds:      &workerv1.Bounds{Classes: classes, MaxSlots: maxSlots},
		OwnerScopes: slices.Clone(ownerScopes),
	}), nil
}

func (r *Router) OpenSession(_ context.Context, req *connect.Request[workerv1.OpenSessionRequest]) (*connect.Response[workerv1.OpenSessionResponse], error) {
	name, err := r.auth(req.Header())
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failOpen > 0 {
		r.failOpen--
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("session active"))
	}
	if r.sess != nil {
		close(r.sess.done)
	}
	r.sess = &session{id: randomHex(16), worker: name, directives: make(chan *workerv1.Directive, 64), done: make(chan struct{})}
	r.sessions++
	r.lastOpen = proto.Clone(req.Msg).(*workerv1.OpenSessionRequest)
	caps := req.Msg.GetCapabilities()
	if caps == nil {
		caps = &workerv1.Capabilities{}
	}
	effective := proto.Clone(caps).(*workerv1.Capabilities)
	if effective.GetSlots() > maxSlots {
		effective.Slots = maxSlots
	}
	rejected := r.rejectOnOpen
	r.rejectOnOpen = nil
	return connect.NewResponse(&workerv1.OpenSessionResponse{
		SessionId:         r.sess.id,
		Effective:         effective,
		Policy:            &workerv1.PublicPolicy{WorkerDefaultAccept: r.defaultAccept},
		HeartbeatInterval: durationpb.New(heartbeatInterval),
		OfferTtl:          durationpb.New(offerTTL),
		RejectedRunners:   rejected,
	}), nil
}

func (r *Router) Subscribe(ctx context.Context, req *connect.Request[workerv1.SubscribeRequest], stream *connect.ServerStream[workerv1.Directive]) error {
	if _, err := r.auth(req.Header()); err != nil {
		return err
	}
	r.mu.Lock()
	s, err := r.sessionLocked(req.Msg.GetSessionId())
	r.mu.Unlock()
	if err != nil {
		return err
	}
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	for {
		var d *workerv1.Directive
		select {
		case <-ctx.Done():
			return nil
		case <-r.closed:
			return nil
		case <-s.done:
			return nil
		case d = <-s.directives:
		case <-keepalive.C:
			d = &workerv1.Directive{Kind: &workerv1.Directive_Keepalive{Keepalive: &workerv1.Keepalive{}}}
		}
		if err := stream.Send(d); err != nil {
			return err
		}
	}
}

func (r *Router) Heartbeat(_ context.Context, req *connect.Request[workerv1.HeartbeatRequest]) (*connect.Response[workerv1.HeartbeatResponse], error) {
	if _, err := r.auth(req.Header()); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.sessionLocked(req.Msg.GetSessionId()); err != nil {
		return nil, err
	}
	r.lastHeartbeat = proto.Clone(req.Msg).(*workerv1.HeartbeatRequest)
	return connect.NewResponse(&workerv1.HeartbeatResponse{Policy: &workerv1.PublicPolicy{WorkerDefaultAccept: r.defaultAccept}}), nil
}

func (r *Router) AcceptOffer(_ context.Context, req *connect.Request[workerv1.AcceptOfferRequest]) (*connect.Response[workerv1.AcceptOfferResponse], error) {
	if _, err := r.auth(req.Header()); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.sessionLocked(req.Msg.GetSessionId()); err != nil {
		return nil, err
	}
	e, ok := r.offers[req.Msg.GetOfferId()]
	if !ok || e.session != req.Msg.GetSessionId() {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("offer not found"))
	}
	delete(r.offers, req.Msg.GetOfferId())
	if time.Now().After(e.offer.GetExpiresAt().AsTime()) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("offer expired"))
	}
	name := e.offer.GetRunnerName()
	r.accepted = append(r.accepted, name)
	repo := e.offer.GetRepo()
	if r.jitRepoOverride != "" {
		repo = r.jitRepoOverride
	}
	gitHubURL, serverURL := "https://github.com/"+repo, defaultServerURL
	if r.jitHostOverride != "" {
		serverURL = r.jitHostOverride
	}
	r.nextRunnerID++
	return connect.NewResponse(&workerv1.AcceptOfferResponse{
		RunnerName:       name,
		RunnerId:         r.nextRunnerID,
		EncodedJitConfig: fakeJIT(gitHubURL, serverURL, name),
	}), nil
}

func (r *Router) RejectOffer(_ context.Context, req *connect.Request[workerv1.RejectOfferRequest]) (*connect.Response[workerv1.RejectOfferResponse], error) {
	if _, err := r.auth(req.Header()); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name, ok := r.offerRunner[req.Msg.GetOfferId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("offer not found"))
	}
	delete(r.offers, req.Msg.GetOfferId())
	r.rejected = append(r.rejected, name)
	return connect.NewResponse(&workerv1.RejectOfferResponse{}), nil
}

func (r *Router) ReportRunner(_ context.Context, req *connect.Request[workerv1.ReportRunnerRequest]) (*connect.Response[workerv1.ReportRunnerResponse], error) {
	if _, err := r.auth(req.Header()); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.sessionLocked(req.Msg.GetSessionId()); err != nil {
		return nil, err
	}
	r.reports = append(r.reports, proto.Clone(req.Msg).(*workerv1.ReportRunnerRequest))
	return connect.NewResponse(&workerv1.ReportRunnerResponse{}), nil
}

func (r *Router) CloseSession(_ context.Context, req *connect.Request[workerv1.CloseSessionRequest]) (*connect.Response[workerv1.CloseSessionResponse], error) {
	if _, err := r.auth(req.Header()); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess != nil && r.sess.id == req.Msg.GetSessionId() {
		close(r.sess.done)
		r.sess = nil
	}
	return connect.NewResponse(&workerv1.CloseSessionResponse{}), nil
}

func (r *Router) auth(h http.Header) (string, error) {
	credential, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	r.mu.Lock()
	defer r.mu.Unlock()
	name, known := r.creds[credential]
	if !ok || !known {
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("unknown worker credential"))
	}
	if r.revoked[name] {
		return "", connect.NewError(connect.CodePermissionDenied, errors.New("worker revoked"))
	}
	return name, nil
}

// sessionLocked returns the live session with this ID; r.mu must be held.
func (r *Router) sessionLocked(id string) (*session, error) {
	if r.sess == nil || r.sess.id != id {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("unknown session"))
	}
	return r.sess, nil
}

func classToProto(c runner.Class) *workerv1.Class {
	pc := &workerv1.Class{}
	switch c.OS {
	case "linux":
		pc.Os = workerv1.OS_OS_LINUX
	case "macos":
		pc.Os = workerv1.OS_OS_MACOS
	case "windows":
		pc.Os = workerv1.OS_OS_WINDOWS
	}
	switch c.Arch {
	case "x64":
		pc.Arch = workerv1.Arch_ARCH_X64
	case "arm64":
		pc.Arch = workerv1.Arch_ARCH_ARM64
	}
	return pc
}

func executorToProto(k runner.ExecutorKind) workerv1.ExecutorKind {
	if k == runner.ExecutorNative {
		return workerv1.ExecutorKind_EXECUTOR_KIND_NATIVE
	}
	return workerv1.ExecutorKind_EXECUTOR_KIND_DOCKER
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "workertest router"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}
