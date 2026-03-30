package control

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Handler processes control requests.
type Handler interface {
	HandleRequest(ctx context.Context, req Request) Response
}

// DefaultTCPAddr is the default TCP address the control server listens on.
// Empty string means TCP is disabled by default (opt-in).
const DefaultTCPAddr = ""

// ControlTokenEnv is the environment variable for the shared control token.
const ControlTokenEnv = "GSO_CONTROL_TOKEN"

// connReadTimeout is the maximum time to wait for a client to send a request.
const connReadTimeout = 30 * time.Second

// Server listens on a Unix domain socket and TCP, dispatching requests to a Handler.
type Server struct {
	socketPath   string
	handler      Handler
	logger       *slog.Logger
	listener     net.Listener
	tcpAddr      string
	tcpListener  net.Listener
	controlToken string
	tlsConfig    *tls.Config
	fingerprint  string
	allowCIDRs   []string     // raw strings, parsed in Start()
	allowNets    []*net.IPNet // parsed CIDRs

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

// WithTLSConfig provides a pre-loaded TLS config (for user-provided certs).
func WithTLSConfig(cfg *tls.Config, fingerprint string) ServerOption {
	return func(s *Server) {
		s.tlsConfig = cfg
		s.fingerprint = fingerprint
	}
}

// WithAllowCIDRs restricts TCP connections to the given CIDRs.
func WithAllowCIDRs(cidrs []string) ServerOption {
	return func(s *Server) {
		s.allowCIDRs = cidrs
	}
}

// NewServer creates a new control server. Options are applied after the
// server is created, so existing callers that pass only three arguments
// continue to work unchanged.
func NewServer(socketPath string, handler Handler, logger *slog.Logger, opts ...ServerOption) *Server {
	s := &Server{
		socketPath:   socketPath,
		tcpAddr:      DefaultTCPAddr,
		controlToken: os.Getenv(ControlTokenEnv),
		handler:      handler,
		logger:       logger,
		done:         make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Fingerprint returns the TLS certificate fingerprint, or empty if TLS is not configured.
func (s *Server) Fingerprint() string {
	return s.fingerprint
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

	// Restrict Unix socket permissions to owner only.
	if err := os.Chmod(s.socketPath, 0700); err != nil {
		_ = ln.Close()
		return fmt.Errorf("setting socket permissions: %w", err)
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
		if s.controlToken == "" {
			s.logger.Warn("TCP listener disabled: GSO_CONTROL_TOKEN is not set")
		} else {
			// Parse CIDR allowlist if configured.
			if len(s.allowCIDRs) > 0 {
				nets, err := ParseCIDRs(s.allowCIDRs)
				if err != nil {
					_ = ln.Close()
					return fmt.Errorf("parsing allow CIDRs: %w", err)
				}
				s.allowNets = nets
			}

			// Auto-generate TLS certificate if not provided via option.
			if s.tlsConfig == nil {
				certDir, err := CertDir()
				if err != nil {
					_ = ln.Close()
					return fmt.Errorf("getting cert directory: %w", err)
				}
				certPath := filepath.Join(certDir, "server.crt")
				keyPath := filepath.Join(certDir, "server.key")
				tlsCfg, fp, err := LoadOrGenerateTLSConfig(certPath, keyPath, s.logger)
				if err != nil {
					_ = ln.Close()
					return fmt.Errorf("loading TLS config: %w", err)
				}
				s.tlsConfig = tlsCfg
				s.fingerprint = fp
			}
			s.logger.Info("TLS certificate fingerprint", "fingerprint", s.fingerprint)

			tcpLn, err := net.Listen("tcp", s.tcpAddr)
			if err != nil {
				_ = ln.Close()
				return fmt.Errorf("listening on TCP %s: %w", s.tcpAddr, err)
			}

			// Wrap the TCP listener with TLS.
			tlsLn := tls.NewListener(tcpLn, s.tlsConfig)

			s.mu.Lock()
			s.tcpListener = tlsLn
			s.mu.Unlock()

			s.logger.Info("control server listening", "tcp", tlsLn.Addr().String())

			go func() {
				select {
				case <-ctx.Done():
					_ = tlsLn.Close()
				case <-s.done:
				}
			}()

			go s.acceptLoop(ctx, tlsLn, true)
		}
	}

	s.acceptLoop(ctx, ln, false)
	return nil
}

// acceptLoop accepts connections on ln until the server is stopped or ctx is cancelled.
// If requireAuth is true, TCP connections must provide a valid control token.
func (s *Server) acceptLoop(ctx context.Context, ln net.Listener, requireAuth bool) {
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

		// Check IP allowlist for TCP connections.
		if requireAuth && len(s.allowNets) > 0 {
			if !CheckIPAllowed(conn.RemoteAddr(), s.allowNets) {
				s.logger.Warn("connection rejected by IP allowlist", "remote", conn.RemoteAddr())
				_ = conn.Close()
				continue
			}
		}

		s.logger.Info("control connection accepted", "remote", conn.RemoteAddr().String(), "network", conn.RemoteAddr().Network())
		go s.handleConn(ctx, conn, requireAuth)
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

func (s *Server) handleConn(ctx context.Context, conn net.Conn, requireAuth bool) {
	defer func() { _ = conn.Close() }()

	// Set overall deadline to prevent slow clients from holding connections open.
	// This covers both reading the request and writing the response.
	_ = conn.SetDeadline(time.Now().Add(connReadTimeout))

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req Request
	if err := dec.Decode(&req); err != nil {
		s.logger.Warn("control request decode error", "remote", conn.RemoteAddr(), "error", err)
		resp := Response{Error: fmt.Sprintf("invalid request: %v", err)}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	if requireAuth && s.controlToken != "" && subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.controlToken)) != 1 {
		s.logger.Warn("control request unauthorized", "remote", conn.RemoteAddr(), "method", req.Method)
		resp := Response{Error: "unauthorized: invalid or missing control token"}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	s.logger.Info("control request", "remote", conn.RemoteAddr(), "method", req.Method)
	resp := s.handler.HandleRequest(ctx, req)
	enc.Encode(resp) //nolint:errcheck
}
