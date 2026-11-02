package worker

import (
	"net/http"
	"time"
)

// SetDockerWaitBackoff shortens the docker launcher's Wait retry backoff for tests.
func SetDockerWaitBackoff(l Launcher, lo, hi time.Duration) {
	d := l.(*dockerLauncher)
	d.backoffMin, d.backoffMax = lo, hi
}

// SetHeartbeatFailureLimit overrides how long heartbeats may fail before the session is dropped.
func SetHeartbeatFailureLimit(c *Client, d time.Duration) { c.hbFailLimit = d }

// TransportHTTP2 exposes the HTTP/2 ping settings of a fresh transport.
func TransportHTTP2(pin string) *http.HTTP2Config { return newTransport(pin).HTTP2 }
