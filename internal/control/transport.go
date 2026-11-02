package control

import "context"

type transportKey struct{}

// WithTransport records whether a request arrived on the local unix socket
// (true) or the TCP listener (false). Server.handleConn sets it.
func WithTransport(ctx context.Context, unixSocket bool) context.Context {
	return context.WithValue(ctx, transportKey{}, unixSocket)
}

// IsUnixSocket reports whether ctx belongs to a request on the local unix
// socket. A context without a recorded transport is not local: worker
// administration fails closed (spec §8.1, SEC-F8).
func IsUnixSocket(ctx context.Context) bool {
	unix, ok := ctx.Value(transportKey{}).(bool)
	return ok && unix
}
