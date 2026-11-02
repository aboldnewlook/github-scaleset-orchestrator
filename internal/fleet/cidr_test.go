package fleet

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDefaultAllowNets(t *testing.T) {
	nets := DefaultAllowNets()
	for _, ok := range []string{"10.1.2.3:5", "172.20.0.4:1", "192.168.1.10:9", "100.64.0.1:1", "127.0.0.1:1", "[::1]:1", "[fd00::1]:1", "[::ffff:192.168.1.10]:1"} {
		if !remoteAllowed(ok, nets) {
			t.Errorf("%s refused by the default allowlist", ok)
		}
	}
	for _, bad := range []string{"8.8.8.8:53", "[2001:db8::1]:1", "172.32.0.1:1", "garbage"} {
		if remoteAllowed(bad, nets) {
			t.Errorf("%s allowed by the default allowlist", bad)
		}
	}
}

func TestAllowCIDRsMiddleware(t *testing.T) {
	_, testNet, _ := net.ParseCIDR("192.0.2.0/24")
	h := allowCIDRs(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), []*net.IPNet{testNet})
	for _, tt := range []struct {
		remote string
		want   int
	}{{"192.0.2.7:1234", http.StatusNoContent}, {"127.0.0.1:1234", http.StatusForbidden}} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = tt.remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s: status %d, want %d", tt.remote, rec.Code, tt.want)
		}
	}
}
