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
	socketPath string
	handler    Handler
	logger     *slog.Logger
	listener   net.Listener

	mu   sync.Mutex
	done chan struct{}
}

// NewServer creates a new control server.
func NewServer(socketPath string, handler Handler, logger *slog.Logger) *Server {
	return &Server{
		socketPath: socketPath,
		handler:    handler,
		logger:     logger,
		done:       make(chan struct{}),
	}
}

// Start begins listening on the Unix socket. It blocks until ctx is cancelled
// or Stop is called.
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

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Check if we're shutting down.
			select {
			case <-s.done:
				return nil
			case <-ctx.Done():
				return nil
			default:
			}
			// Transient error — log and continue.
			if !errors.Is(err, net.ErrClosed) {
				s.logger.Error("accept error", "error", err)
			}
			return nil
		}

		go s.handleConn(ctx, conn)
	}
}

// Stop closes the listener and removes the socket file.
func (s *Server) Stop() error {
	s.mu.Lock()
	ln := s.listener
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
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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
