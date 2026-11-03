package cmd

import (
	"strings"
	"testing"
)

func TestScalesetResetArgsAndFlags(t *testing.T) {
	if err := scalesetResetCmd.Args(scalesetResetCmd, []string{"o/r"}); err == nil {
		t.Error("reset accepted one argument; want <repo> <class>")
	}
	if err := scalesetResetCmd.Args(scalesetResetCmd, []string{"o/r", "linux-x64", "extra"}); err == nil {
		t.Error("reset accepted three arguments")
	}
	if err := scalesetResetCmd.Args(scalesetResetCmd, []string{"o/r", "linux-x64"}); err != nil {
		t.Errorf("reset rejected <repo> <class>: %v", err)
	}
	if !strings.HasPrefix(scalesetResetCmd.Use, "reset <repo> <class>") {
		t.Errorf("Use = %q", scalesetResetCmd.Use)
	}
	if scalesetResetCmd.Flags().Lookup("force") == nil {
		t.Error("reset has no --force")
	}
	if scalesetListCmd.Flags().Lookup("all") == nil {
		t.Error("list has no --all")
	}
}

func TestScalesetResetRejectsBadClassBeforeAnythingElse(t *testing.T) {
	// No config file, no stdin prompt: a bad class must fail first.
	err := runScalesetReset(scalesetResetCmd, []string{"o/r", "plan9-x64"})
	if err == nil || !strings.Contains(err.Error(), "plan9-x64") {
		t.Fatalf("runScalesetReset(bad class) = %v", err)
	}
}
