package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// dockerEngine is a fake Docker engine whose /wait behaviour the test scripts.
type dockerEngine struct {
	waits   atomic.Int32
	release chan struct{} // closed to let the successful wait proceed
	// failures is how many /wait calls fail with a 500 ("engine down") first.
	failures int32
	// waitStatus is the exit status the successful wait reports; notFound makes
	// every wait answer 404 instead.
	waitStatus int
	notFound   bool
}

func (e *dockerEngine) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1.43/version":
		_ = json.NewEncoder(w).Encode(map[string]string{"Arch": "amd64"})
	case r.URL.Path == "/v1.43/containers/create":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"Id":"cid-1"}`)
	case strings.HasSuffix(r.URL.Path, "/start"):
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(r.URL.Path, "/wait"):
		n := e.waits.Add(1)
		switch {
		case e.notFound:
			w.WriteHeader(http.StatusNotFound)
		case n <= e.failures:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"engine restarting"}`)
		default:
			select {
			case <-e.release:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": e.waitStatus})
		}
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func exitedReports(h *harness, name string) int {
	n := 0
	for _, rep := range h.router.Reports() {
		if rep.GetRunnerName() == name && rep.GetPhase() == phaseExited {
			n++
		}
	}
	return n
}

// A Docker engine that is down while the worker waits must not look like an
// exit: the record stays, nothing is reported, and the real exit is reported once.
func TestClientKeepsRunnerWhileDockerEngineIsDown(t *testing.T) {
	eng := &dockerEngine{release: make(chan struct{}), failures: 2, waitStatus: 3}
	launcher := newDockerLauncher(t, fakeEngine(t, eng.handler))
	worker.SetDockerWaitBackoff(launcher, time.Millisecond, 5*time.Millisecond)
	h := newHarness(t)
	h.start(h.config(func(c *worker.Config) { c.Launchers = []worker.Launcher{launcher} }))

	_, name := h.router.Offer("acme/app", "linux-x64", false)
	h.router.WaitPhase(t, name, phaseRunning, waitTimeout)
	waitFor(t, "third wait attempt", func() bool { return eng.waits.Load() >= 3 })

	if recs := h.records(); len(recs) != 1 || recs[0].Name != name {
		t.Fatalf("records while the engine was down = %+v, want %s kept", recs, name)
	}
	if n := exitedReports(h, name); n != 0 {
		t.Fatalf("%d EXITED reports while the container was still running", n)
	}

	close(eng.release)
	h.router.WaitPhase(t, name, phaseExited, waitTimeout)
	waitFor(t, "record removed", func() bool { return len(h.records()) == 0 })
	time.Sleep(50 * time.Millisecond)
	if n := exitedReports(h, name); n != 1 {
		t.Fatalf("EXITED reports = %d, want exactly 1", n)
	}
	if rep := reportFor(h.router, name, phaseExited); rep.GetExitCode() != 3 {
		t.Fatalf("exit report = %v, want the real exit code 3", rep)
	}
}

func TestClientFinishesRunnerWhoseContainerIsGone(t *testing.T) {
	eng := &dockerEngine{release: make(chan struct{}), notFound: true}
	launcher := newDockerLauncher(t, fakeEngine(t, eng.handler))
	worker.SetDockerWaitBackoff(launcher, time.Millisecond, 5*time.Millisecond)
	h := newHarness(t)
	h.start(h.config(func(c *worker.Config) { c.Launchers = []worker.Launcher{launcher} }))

	_, name := h.router.Offer("acme/app", "linux-x64", false)
	h.router.WaitPhase(t, name, phaseExited, waitTimeout)
	waitFor(t, "record removed", func() bool { return len(h.records()) == 0 })
	if rep := reportFor(h.router, name, phaseExited); rep.GetError() == "" {
		t.Fatalf("exit report = %v, want an error for the vanished container", rep)
	}
	if got := eng.waits.Load(); got != 1 {
		t.Fatalf("wait attempts = %d, want 1 (a missing container is not retried)", got)
	}
}

// freezeProxy forwards TCP to target; freeze() makes every connection open so
// far swallow its traffic without closing, like a dead NAT mapping.
type freezeProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []*atomic.Bool
}

func newFreezeProxy(t *testing.T, target string) *freezeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezeProxy{ln: ln, target: target}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(t, c)
		}
	}()
	return p
}

func (p *freezeProxy) addr() string { return p.ln.Addr().String() }

func (p *freezeProxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.conns {
		f.Store(true)
	}
}

func (p *freezeProxy) serve(t *testing.T, c net.Conn) {
	up, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = c.Close()
		return
	}
	frozen := &atomic.Bool{}
	p.mu.Lock()
	p.conns = append(p.conns, frozen)
	p.mu.Unlock()
	t.Cleanup(func() { _ = c.Close(); _ = up.Close() })
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 && !frozen.Load() {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go pipe(up, c)
	pipe(c, up)
}

// A connection that goes silent without closing must not strand the worker:
// heartbeats failing past the limit end the session and it reconnects afresh.
func TestClientReconnectsWhenTheConnectionGoesSilent(t *testing.T) {
	h := newHarness(t)
	proxy := newFreezeProxy(t, h.state.Addr)
	cfg := h.config(func(c *worker.Config) { c.DialAddr = proxy.addr() })
	c, err := worker.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	worker.SetHeartbeatFailureLimit(c, 300*time.Millisecond)
	c.SetAvailability(worker.Available)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v after cancel", err)
		}
	})
	waitFor(t, "first session", func() bool { return h.router.Sessions() == 1 && c.Status().Connected })

	proxy.freeze()
	waitFor(t, "reconnect on a fresh connection", func() bool { return h.router.Sessions() >= 2 })
	waitFor(t, "connected again", func() bool { return c.Status().Connected })
}

func TestTransportSendsHTTP2LivenessPings(t *testing.T) {
	cfg := worker.TransportHTTP2("pin")
	if cfg == nil || cfg.SendPingTimeout != 15*time.Second || cfg.PingTimeout != 10*time.Second {
		t.Fatalf("HTTP2 config = %+v, want SendPingTimeout 15s and PingTimeout 10s", cfg)
	}
}

// A drain deadline that fires after Run returned must not kill runners the
// next Run re-attaches to.
func TestClientStopCancelsPendingDrainDeadline(t *testing.T) {
	h := newHarness(t)
	c, stop := h.start(h.config())
	_, name := h.router.Offer("acme/app", "linux-x64", false)
	h.router.WaitPhase(t, name, phaseRunning, waitTimeout)

	c.Drain(time.Now().Add(200 * time.Millisecond))
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if got := h.launch.Killed(); len(got) != 0 {
		t.Fatalf("a drain deadline firing after shutdown killed %v", got)
	}
	if recs := h.records(); len(recs) != 1 || !slices.Equal([]string{recs[0].Name}, []string{name}) {
		t.Fatalf("records = %+v, want %s kept", recs, name)
	}
}
