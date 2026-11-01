package worker

import (
	"errors"
	"fmt"
	"net"
	"regexp"
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
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return Join{}, fmt.Errorf("join string address %q must be host:port", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return Join{}, fmt.Errorf("join string address %q has an invalid port", addr)
	}
	return Join{Addr: addr, Pin: pin, Code: code}, nil
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
