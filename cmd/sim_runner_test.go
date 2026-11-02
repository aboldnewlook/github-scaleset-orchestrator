package cmd

import (
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
)

func TestIsSimJIT(t *testing.T) {
	t.Setenv(simJITEnv, ghsim.EncodeJIT("https://github.com/o/a", "http://sim:9300", "gso-x-a-abc123", "tok"))
	if err := runIsSimJIT(nil, nil); err != nil {
		t.Fatalf("simulator JIT: %v", err)
	}
	t.Setenv(simJITEnv, "eyJub3QiOiJzaW0ifQ==") // {"not":"sim"}
	if err := runIsSimJIT(nil, nil); err == nil {
		t.Fatal("a non-simulator JIT was accepted")
	}
	t.Setenv(simJITEnv, "")
	if err := runIsSimJIT(nil, nil); err == nil {
		t.Fatal("an empty JIT was accepted")
	}
}
