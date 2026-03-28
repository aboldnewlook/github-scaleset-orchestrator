package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
)

// Handler processes control requests.
type Handler interface {
	HandleRequest(ctx context.Context, req Request) Response
}

// Server listens on a Unix domain socket and dispatches requests to a Handler.
type Server struct {
	socketPath  string
	handler     Handler
	logger      *slog.Logger
	listener    net.Listener
	tcpAddr     string
	tcpListener net.Listener

	mu   sync.Mutex
	done chan struct{}
}

// ServerOption configures optional Server behavior.
type ServerOption func(*Server)

// WithTCPAddr configures the server to also listen on a TCP address (e.g. ":9100").
func WithTCPAddr(addr string) ServerOption {
	return func(s *Server) {
		s.tcpAddr = addr
	}
}

// NewServer creates a new control server. Options are applied after the
// server is created, so existing callers that pass only three arguments
// continue to work unchanged.
func NewServer(socketPath string, handler Handler, logger *slog.Logger, opts ...ServerOption) *Server {
	s := &Server{
		socketPath: socketPath,
		handler:    handler,
		logger:     logger,
		done:       make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start begins listening on the Unix socket (and optionally TCP). It blocks
// until ctx is cancelled or Stop is called.
func (s *Server) Start(ctx context.Context) error {
	if err := s.checkStaleSocket(); err != nil {
		return err
	}

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.socketPath, err)
	}

	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	s.logger.Info("control server listening", "socket", s.socketPath)

	// Close the listener when context is done so Accept unblocks.
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-s.done:
		}
	}()

	// Start TCP listener if configured.
	if s.tcpAddr != "" {
		tcpLn, err := net.Listen("tcp", s.tcpAddr)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("listening on TCP %s: %w", s.tcpAddr, err)
		}

		s.mu.Lock()
		s.tcpListener = tcpLn
		s.mu.Unlock()

		s.logger.Info("control server listening", "tcp", tcpLn.Addr().String())

		go func() {
			select {
			case <-ctx.Done():
				_ = tcpLn.Close()
			case <-s.done:
			}
		}()

		go s.acceptLoop(ctx, tcpLn)
	}

	s.acceptLoop(ctx, ln)
	return nil
}

// acceptLoop accepts connections on ln until the server is stopped or ctx is cancelled.
func (s *Server) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Check if we're shutting down.
			select {
			case <-s.done:
				return
			case <-ctx.Done():
				return
			default:
			}
			// Transient error — log and continue.
			if !errors.Is(err, net.ErrClosed) {
				s.logger.Error("accept error", "error", err)
			}
			return
		}

		go s.handleConn(ctx, conn)
	}
}

// Stop closes the listeners and removes the socket file.
func (s *Server) Stop() error {
	s.mu.Lock()
	ln := s.listener
	tcpLn := s.tcpListener
	s.mu.Unlock()

	select {
	case <-s.done:
		// Already stopped.
		return nil
	default:
		close(s.done)
	}

	var errs []error
	if ln != nil {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	if tcpLn != nil {
		if err := tcpLn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// TCPAddr returns the TCP listener's address, or an empty string if TCP is
// not enabled. This is useful when the server was started with ":0" to let
// the OS pick a free port.
func (s *Server) TCPAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tcpListener != nil {
		return s.tcpListener.Addr().String()
	}
	return ""
}

// checkStaleSocket detects whether a socket file already exists. If it does,
// it tries to connect. If connection is refused, the socket is stale and gets
// removed. If connection succeeds, another instance is already running.
func (s *Server) checkStaleSocket() error {
	if _, err := os.Stat(s.socketPath); os.IsNotExist(err) {
		return nil
	}

	conn, err := net.Dial("unix", s.socketPath)
	if err != nil {
		// Connection refused or failed — stale socket, remove it.
		s.logger.Info("removing stale socket", "socket", s.socketPath)
		return os.Remove(s.socketPath)
	}
	_ = conn.Close()
	return fmt.Errorf("another instance is already running (socket %s is active)", s.socketPath)
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req Request
	if err := dec.Decode(&req); err != nil {
		resp := Response{Error: fmt.Sprintf("invalid request: %v", err)}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	resp := s.handler.HandleRequest(ctx, req)
	enc.Encode(resp) //nolint:errcheck
}
