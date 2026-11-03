//go:build !windows

// Package e2e runs a simulated GitHub, a router and workers as real gso
// processes talking over loopback TLS.
package e2e

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var gsoBin string

func TestMain(m *testing.M) {
	flag.Parse()
	// Windows is excluded by the //go:build !windows line on every file here
	// (the sim executor needs process groups).
	if testing.Short() {
		fmt.Println("e2e: skipped in -short mode")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "gsoe2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gsoBin = filepath.Join(dir, "gso")
	wd, _ := os.Getwd()
	build := exec.Command("go", "build", "-o", gsoBin, ".")
	build.Dir = filepath.Join(wd, "..", "..")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building gso:", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
