package router

import (
	"io"
	"net"
	"testing"
	"time"
)

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestWorkerListenerClosesDisallowedPeersBeforeTLS(t *testing.T) {
	_, only10, _ := net.ParseCIDR("10.0.0.0/8")
	ln := newWorkerListener(listenLoopback(t), []*net.IPNet{only10}, 8)
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// Loopback is outside 10.0.0.0/8: the router closes it without reading
	// or writing a byte (no TLS handshake).
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("read after a disallowed connect = %v, want a clean close", err)
	}
	select {
	case <-accepted:
		t.Fatal("a disallowed peer reached Accept")
	default:
	}
}

func TestWorkerListenerCapsConnections(t *testing.T) {
	ln := newWorkerListener(listenLoopback(t), nil, 2) // default allowlist includes loopback
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	dial()
	dial()
	dial()
	var held []net.Conn
	for range 2 {
		select {
		case c := <-accepted:
			held = append(held, c)
		case <-time.After(2 * time.Second):
			t.Fatal("first two connections not accepted")
		}
	}
	select {
	case <-accepted:
		t.Fatal("a third connection was accepted beyond the cap of 2")
	case <-time.After(200 * time.Millisecond):
	}
	_ = held[0].Close() // frees a slot
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("the queued connection was not accepted after a slot freed")
	}
}
