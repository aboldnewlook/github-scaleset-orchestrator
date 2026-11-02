package worker

import (
	"errors"
	"fmt"
	"testing"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

func TestClassProtoRoundTrip(t *testing.T) {
	for _, c := range runner.AllClasses {
		got, err := classFromProto(classToProto(c))
		if err != nil || got != c {
			t.Errorf("round trip of %s = %s, %v", c, got, err)
		}
	}
	for _, pc := range []*workerv1.Class{nil, {}, {Os: workerv1.OS_OS_LINUX}} {
		if _, err := classFromProto(pc); err == nil {
			t.Errorf("classFromProto(%v) accepted an incomplete class", pc)
		}
	}
}

func TestExecutorKindProtoRoundTrip(t *testing.T) {
	for _, k := range []runner.ExecutorKind{runner.ExecutorDocker, runner.ExecutorNative} {
		if got := kindFromProto(kindToProto(k)); got != k {
			t.Errorf("round trip of %s = %q", k, got)
		}
	}
	if got := kindFromProto(workerv1.ExecutorKind_EXECUTOR_KIND_UNSPECIFIED); got != "" {
		t.Errorf("unspecified kind = %q, want empty", got)
	}
}

func TestExitCode(t *testing.T) {
	status3 := errors.New("runner container exited with status 3")
	tests := []struct {
		err  error
		want int32
	}{
		{nil, 0},
		{status3, 3},
		{fmt.Errorf("waiting: %w", status3), 3},
		{errors.New("docker API 500: boom"), 1},
	}
	for _, tt := range tests {
		if got := exitCode(tt.err); got != tt.want {
			t.Errorf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

func TestSlotsToProtoIsBounded(t *testing.T) {
	for in, want := range map[int]uint32{-1: 0, 0: 0, 3: 3, maxSlots + 10: maxSlots} {
		if got := slotsToProto(in); got != want {
			t.Errorf("slotsToProto(%d) = %d, want %d", in, got, want)
		}
	}
}
