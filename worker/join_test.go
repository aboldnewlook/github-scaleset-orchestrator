package worker

import (
	"strings"
	"testing"
)

const (
	testPin  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testCode = "fedcba9876543210fedcba9876543210"
)

func TestParseJoin(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Join
		wantErr string
	}{
		{"hostname", "gso-join:v1:unraid.lan:9200:" + testPin + ":" + testCode, Join{Addr: "unraid.lan:9200", Pin: testPin, Code: testCode}, ""},
		{"IPv6", "gso-join:v1:[fd00::1]:9200:" + testPin + ":" + testCode, Join{Addr: "[fd00::1]:9200", Pin: testPin, Code: testCode}, ""},
		{"surrounding space and uppercase hex", "  gso-join:v1:10.0.0.5:9200:" + strings.ToUpper(testPin) + ":" + strings.ToUpper(testCode) + "\n", Join{Addr: "10.0.0.5:9200", Pin: testPin, Code: testCode}, ""},
		{"wrong prefix", "gso-join:v2:unraid.lan:9200:" + testPin + ":" + testCode, Join{}, "must start with"},
		{"short pin", "gso-join:v1:unraid.lan:9200:abcd:" + testCode, Join{}, "pin must be 64 hex"},
		{"short code", "gso-join:v1:unraid.lan:9200:" + testPin + ":abcd", Join{}, "code must be 32 hex"},
		{"no port", "gso-join:v1:unraid.lan:" + testPin + ":" + testCode, Join{}, "must be host:port"},
		{"bad port", "gso-join:v1:unraid.lan:99999:" + testPin + ":" + testCode, Join{}, "invalid port"},
		{"IPv4", "gso-join:v1:203.0.113.5:9200:" + testPin + ":" + testCode, Join{Addr: "203.0.113.5:9200", Pin: testPin, Code: testCode}, ""},
		{"userinfo in host", "gso-join:v1:unraid.lan@203.0.113.5:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"path in host", "gso-join:v1:unraid.lan/x:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"query in host", "gso-join:v1:unraid.lan?x:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"fragment in host", "gso-join:v1:unraid.lan#x:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"backslash in host", "gso-join:v1:unraid.lan\\x:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"space in host", "gso-join:v1:unraid lan:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"zone in IPv6", "gso-join:v1:[fe80::1%en0]:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"empty label", "gso-join:v1:unraid..lan:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"leading dot", "gso-join:v1:.unraid.lan:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"underscore host", "gso-join:v1:un_raid:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"bracketed non-IP", "gso-join:v1:[unraid.lan]:9200:" + testPin + ":" + testCode, Join{}, "invalid host"},
		{"port zero", "gso-join:v1:unraid.lan:0:" + testPin + ":" + testCode, Join{}, "invalid port"},
		{"port 70000", "gso-join:v1:unraid.lan:70000:" + testPin + ":" + testCode, Join{}, "invalid port"},
		{"port non-numeric", "gso-join:v1:unraid.lan:http:" + testPin + ":" + testCode, Join{}, "invalid port"},
		{"nothing after prefix", "gso-join:v1:", Join{}, "missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseJoin(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseJoin() error = %v, want %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), testCode) {
					t.Fatal("error message leaks the enrollment code")
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseJoin() = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestFormatJoinRoundTripsAndStringRedacts(t *testing.T) {
	s := FormatJoin("unraid.lan:9200", testPin, testCode)
	j, err := ParseJoin(s)
	if err != nil {
		t.Fatal(err)
	}
	if FormatJoin(j.Addr, j.Pin, j.Code) != s {
		t.Fatalf("round trip changed %q", s)
	}
	if strings.Contains(j.String(), testCode) || !strings.Contains(j.String(), "unraid.lan:9200") {
		t.Fatalf("String() = %q, want the address and no code", j.String())
	}
}

func TestShortPin(t *testing.T) {
	if got := ShortPin(testPin); got != "0123 4567 89ab cdef" {
		t.Fatalf("ShortPin() = %q", got)
	}
	if got := ShortPin("ABCDEF"); got != "abcd ef" {
		t.Fatalf("ShortPin(short) = %q", got)
	}
}

func TestParseJoinErrorsNeverEchoAddress(t *testing.T) {
	// A mangled string can put the code where the address belongs.
	for _, in := range []string{
		"gso-join:v1:" + testCode + ":" + testPin + ":" + testCode,
		"gso-join:v1:" + testCode + ":99999:" + testPin + ":" + testCode,
	} {
		_, err := ParseJoin(in)
		if err == nil {
			t.Fatalf("ParseJoin(%q) succeeded", in)
		}
		if strings.Contains(err.Error(), testCode) {
			t.Fatalf("error leaks the code: %v", err)
		}
	}
}

func TestValidJoinAddr(t *testing.T) {
	for addr, ok := range map[string]bool{
		"unraid.lan:9200": true, "10.0.0.5:9200": true, "[fd00::1]:9200": true,
		"my_nas:9200": false, "unraid.lan.:9200": false, "a@b:9200": false,
		"unraid.lan:0": false, "unraid.lan": false, ":9200": false,
	} {
		if err := ValidJoinAddr(addr); (err == nil) != ok {
			t.Errorf("ValidJoinAddr(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}
