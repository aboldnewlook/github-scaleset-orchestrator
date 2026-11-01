package runner

import "testing"

func TestParseClass(t *testing.T) {
	tests := []struct {
		in      string
		want    Class
		wantErr bool
	}{
		{"linux-x64", Class{"linux", "x64"}, false},
		{"Linux-ARM64", Class{"linux", "arm64"}, false},
		{" macos-arm64 ", Class{"macos", "arm64"}, false},
		{"windows-x64", Class{"windows", "x64"}, false},
		{"windows-arm64", Class{"windows", "arm64"}, false},
		{"darwin-arm64", Class{}, true},
		{"linux-amd64", Class{}, true},
		{"linux", Class{}, true},
		{"", Class{}, true},
		{"linux-x64-extra", Class{}, true},
	}
	for _, tt := range tests {
		got, err := ParseClass(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseClass(%q) = %+v, %v; want %+v, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestClassForMapsGoNames(t *testing.T) {
	tests := []struct {
		goos, goarch string
		want         string
		wantErr      bool
	}{
		{"linux", "amd64", "linux-x64", false},
		{"linux", "arm64", "linux-arm64", false},
		{"darwin", "arm64", "macos-arm64", false},
		{"darwin", "amd64", "macos-x64", false},
		{"windows", "amd64", "windows-x64", false},
		{"windows", "arm64", "windows-arm64", false},
		{"freebsd", "amd64", "", true},
		{"linux", "386", "", true},
	}
	for _, tt := range tests {
		got, err := ClassFor(tt.goos, tt.goarch)
		if (err != nil) != tt.wantErr || got.String() != tt.want {
			t.Errorf("ClassFor(%s, %s) = %q, %v; want %q, err=%v", tt.goos, tt.goarch, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestClassLabelsAndExecutor(t *testing.T) {
	tests := []struct {
		class         Class
		str, os, arch string
		executor      ExecutorKind
	}{
		{Class{"linux", "x64"}, "linux-x64", "Linux", "X64", ExecutorDocker},
		{Class{"linux", "arm64"}, "linux-arm64", "Linux", "ARM64", ExecutorDocker},
		{Class{"macos", "x64"}, "macos-x64", "macOS", "X64", ExecutorNative},
		{Class{"macos", "arm64"}, "macos-arm64", "macOS", "ARM64", ExecutorNative},
		{Class{"windows", "x64"}, "windows-x64", "Windows", "X64", ExecutorNative},
		{Class{"windows", "arm64"}, "windows-arm64", "Windows", "ARM64", ExecutorNative},
	}
	if len(tests) != len(AllClasses) {
		t.Fatalf("test covers %d classes, AllClasses has %d", len(tests), len(AllClasses))
	}
	for _, tt := range tests {
		c := tt.class
		if c.String() != tt.str || c.OSLabel() != tt.os || c.ArchLabel() != tt.arch || c.Executor() != tt.executor {
			t.Errorf("%+v: got %q %q %q %q", c, c.String(), c.OSLabel(), c.ArchLabel(), c.Executor())
		}
		back, err := ParseClass(c.String())
		if err != nil || back != c {
			t.Errorf("ParseClass(%q) = %+v, %v; want %+v", c.String(), back, err, c)
		}
	}
}

func TestHostClassIsSupported(t *testing.T) {
	c := HostClass()
	if c.IsZero() {
		t.Fatal("HostClass() is zero")
	}
	if _, err := ParseClass(c.String()); err != nil {
		t.Fatalf("HostClass() = %q does not parse: %v", c, err)
	}
}

func TestZeroClass(t *testing.T) {
	var c Class
	if !c.IsZero() || c.String() != "" {
		t.Fatalf("zero Class: IsZero=%v String=%q", c.IsZero(), c.String())
	}
}
