package worker

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// JoinPrefix starts every join string `gso workers add` prints.
const JoinPrefix = "gso-join:v1:"

// Join is a parsed join string: where the router is, which key it holds, and a
// single-use enrollment code (design spec §4.4). The join string is the root of
// trust, so transfer it over a channel you trust.
type Join struct {
	Addr string // router host:port (workers.advertise)
	Pin  string // lowercase hex SHA-256 of the router key's SubjectPublicKeyInfo
	Code string // 128-bit single-use enrollment code; a secret
}

var (
	pinPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	codePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ParseJoin parses "gso-join:v1:<host:port>:<pin>:<code>". The pin and the code
// are read from the right, so an IPv6 address such as [fd00::1]:9200 works.
// Error messages never include the code.
func ParseJoin(s string) (Join, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, JoinPrefix) {
		return Join{}, fmt.Errorf("not a gso join string: it must start with %q", JoinPrefix)
	}
	rest := s[len(JoinPrefix):]
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return Join{}, errors.New("join string is missing its enrollment code")
	}
	code := strings.ToLower(rest[i+1:])
	rest = rest[:i]
	i = strings.LastIndexByte(rest, ':')
	if i < 0 {
		return Join{}, errors.New("join string is missing the router key pin")
	}
	pin := strings.ToLower(rest[i+1:])
	addr := rest[:i]
	if !pinPattern.MatchString(pin) {
		return Join{}, errors.New("join string pin must be 64 hex characters")
	}
	if !codePattern.MatchString(code) {
		return Join{}, errors.New("join string enrollment code must be 32 hex characters")
	}
	if err := ValidJoinAddr(addr); err != nil {
		return Join{}, fmt.Errorf("join string %w", err)
	}
	return Join{Addr: addr, Pin: pin, Code: code}, nil
}

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

// validJoinHost accepts an IPv4 literal, a bracketed IPv6 literal without a
// zone, or a DNS name with no empty labels. The host is dialed and shown to the
// user, so userinfo, paths, queries and whitespace must not get through.
func validJoinHost(addr, host string) bool {
	if strings.HasPrefix(addr, "[") {
		ip := net.ParseIP(host)
		return ip != nil && strings.Contains(host, ":")
	}
	if !hostPattern.MatchString(host) {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4() != nil
	}
	return !slices.Contains(strings.Split(host, "."), "")
}

// FormatJoin builds a join string. The router calls it in `gso workers add`.
func FormatJoin(addr, pin, code string) string {
	return JoinPrefix + addr + ":" + pin + ":" + code
}

// String returns the join string with the enrollment code redacted, so a Join
// can be printed or logged. Use FormatJoin for the real string.
func (j Join) String() string {
	return JoinPrefix + j.Addr + ":" + j.Pin + ":<redacted>"
}

// ShortPin formats the first 16 hex characters of a pin in groups of four, for
// the "is this your router?" confirmation (design spec §8.6).
func ShortPin(pin string) string {
	pin = strings.ToLower(pin)
	if len(pin) > 16 {
		pin = pin[:16]
	}
	var groups []string
	for len(pin) > 4 {
		groups = append(groups, pin[:4])
		pin = pin[4:]
	}
	if pin != "" {
		groups = append(groups, pin)
	}
	return strings.Join(groups, " ")
}

// ValidJoinAddr checks a router "host:port" the way ParseJoin does, so a router
// never mints a join string that every worker rejects.
func ValidJoinAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return errors.New("address must be host:port")
	}
	if !validJoinHost(addr, host) {
		return errors.New("address has an invalid host: use a DNS name, an IPv4 address or a bracketed IPv6 address")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("address has an invalid port")
	}
	return nil
}
