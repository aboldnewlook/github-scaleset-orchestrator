package fleet

import (
	"net"
	"net/http"
)

// DefaultAllowNets is the worker listener's default source allowlist (spec
// §4.2): private ranges only. It is deliberately not "the router's subnet",
// which inside a container is the docker bridge.
func DefaultAllowNets() []*net.IPNet {
	cidrs := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "127.0.0.0/8", "fc00::/7", "::1/128"}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err) // constant input
		}
		nets = append(nets, n)
	}
	return nets
}

func remoteAllowed(remoteAddr string, nets []*net.IPNet) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// allowCIDRs refuses connections from outside nets with a bare 403.
func allowCIDRs(next http.Handler, nets []*net.IPNet) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !remoteAllowed(r.RemoteAddr, nets) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
