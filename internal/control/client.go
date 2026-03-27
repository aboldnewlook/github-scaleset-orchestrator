package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
)

// Client connects to the control server over a Unix socket.
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

// Call sends a request to the daemon and returns the result.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	req := Request{Method: method}

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
