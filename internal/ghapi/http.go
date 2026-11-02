package ghapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/actions/scaleset"
)

const (
	// A dead HTTP/2 connection once stalled a listener for about 25 minutes
	// (scaleset#131). Every listener shares one pool, so health-check it.
	http2SendPingTimeout = 30 * time.Second
	http2PingTimeout     = 15 * time.Second
)

// NewHTTPClient is the HTTP client every listener and REST call shares: the
// scaleset SDK's default transport, with HTTP/2 health checks (design spec
// §5.3). Each GetMessage poll is bounded by scaleset.MessagePollTimeout.
func NewHTTPClient() *http.Client {
	t := scaleset.DefaultTransport()
	t.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: http2SendPingTimeout,
		PingTimeout:     http2PingTimeout,
	}
	return &http.Client{Transport: t, Timeout: scaleset.DefaultTimeout}
}

// IsConflict reports whether err is a scale-set API 409, e.g. a message
// session another owner still holds. It replaces matching on "409".
func IsConflict(err error) bool {
	return errors.Is(err, scaleset.ConflictError)
}
