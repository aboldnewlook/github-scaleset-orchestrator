package control

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
)

// ClientOption configures optional Client behavior.
type ClientOption func(*clientConfig)

type clientConfig struct {
	knownHostsPath   string
	trustFingerprint string
}

// WithTrustFingerprint pre-trusts a specific server fingerprint (for automation).
func WithTrustFingerprint(fp string) ClientOption {
	return func(c *clientConfig) {
		c.trustFingerprint = fp
	}
}

// WithKnownHostsPath overrides the default known_hosts file path.
func WithKnownHostsPath(path string) ClientOption {
	return func(c *clientConfig) {
		c.knownHostsPath = path
	}
}

// Client connects to the control server over a Unix socket or TCP.
type Client struct {
	conn net.Conn
}

// NewClient connects to the daemon's control socket using SocketPath().
// Returns a clear error if the daemon is not running.
func NewClient() (*Client, error) {
	return NewClientWithPath(SocketPath())
}

// NewClientWithPath connects to a specific socket path.
func NewClientWithPath(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("daemon is not running")
	}
	return &Client{conn: conn}, nil
}

// NewClientWithAddr dials a TCP address using TLS with TOFU verification.
func NewClientWithAddr(addr string, opts ...ClientOption) (*Client, error) {
	cfg := &clientConfig{}
	for _, o := range opts {
		o(cfg)
	}

	if cfg.knownHostsPath == "" {
		var err error
		cfg.knownHostsPath, err = KnownHostsPath()
		if err != nil {
			return nil, fmt.Errorf("resolving known_hosts path: %w", err)
		}
	}

	tlsCfg := TOFUTLSConfig(cfg.knownHostsPath, addr, cfg.trustFingerprint)
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to remote daemon at %s: %w", addr, err)
	}
	return &Client{conn: conn}, nil
}

// Connect returns a client connected to a remote TCP address if addr is
// non-empty, otherwise falls back to the local Unix socket.
func Connect(addr string, opts ...ClientOption) (*Client, error) {
	if addr != "" {
		return NewClientWithAddr(addr, opts...)
	}
	return NewClient()
}

// Call sends a request to the daemon and returns the result.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	req := Request{
		Method: method,
		Token:  os.Getenv(ControlTokenEnv),
	}

	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshaling params: %w", err)
		}
		req.Params = p
	}

	enc := json.NewEncoder(c.conn)
	if err := enc.Encode(req); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	dec := json.NewDecoder(c.conn)
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("server error: %s", resp.Error)
	}

	return resp.Result, nil
}

// Close closes the connection to the daemon.
func (c *Client) Close() error {
	return c.conn.Close()
}
