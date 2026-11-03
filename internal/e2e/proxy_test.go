//go:build !windows

package e2e

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// proxy forwards TCP to target and can sever every connection on demand.
type proxy struct {
	ln     net.Listener
	target string

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	blocked time.Time
}

func newProxy(t *testing.T, target string) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln, target: target, conns: make(map[net.Conn]struct{})}
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.closeAll()
	})
	return p
}

func (p *proxy) Addr() string { return p.ln.Addr().String() }

func (p *proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		blocked := time.Now().Before(p.blocked)
		p.mu.Unlock()
		if blocked {
			_ = c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns[c], p.conns[up] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go p.pipe(c, up)
		go p.pipe(up, c)
	}
}

func (p *proxy) pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}

// Sever closes every open connection and refuses new ones for d.
func (p *proxy) Sever(d time.Duration) {
	p.mu.Lock()
	p.blocked = time.Now().Add(d)
	p.mu.Unlock()
	p.closeAll()
}

func (p *proxy) closeAll() {
	p.mu.Lock()
	conns := p.conns
	p.conns = make(map[net.Conn]struct{})
	p.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
}
