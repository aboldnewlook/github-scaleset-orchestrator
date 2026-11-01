package workerv1_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
)

func TestOfferDirectiveRoundTripKeepsEveryField(t *testing.T) {
	in := &workerv1.Directive{Kind: &workerv1.Directive_Offer{Offer: &workerv1.Offer{
		OfferId:    "0123456789abcdef0123456789abcdef",
		RunnerName: "gso-unraid-prune-a1b2c3",
		Repo:       "jrschumacher/prune",
		Class:      &workerv1.Class{Os: workerv1.OS_OS_LINUX, Arch: workerv1.Arch_ARCH_X64},
		Executor:   workerv1.ExecutorKind_EXECUTOR_KIND_DOCKER,
		PublicRepo: true,
		ExpiresAt:  timestamppb.New(time.Unix(1_700_000_000, 0)),
		CacheMode:  "repo",
	}}}
	data, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out workerv1.Directive
	if err := proto.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, &out) {
		t.Fatalf("round trip changed the directive:\n in: %v\nout: %v", in, &out)
	}
	if got := out.GetOffer().GetCacheMode(); got != "repo" {
		t.Fatalf("cache_mode = %q, want repo", got)
	}
}

// The wire numbers are the compatibility contract (spec §4.6: v1 changes are
// additive only). Pin the ones other tasks and the Burst app depend on.
func TestFieldNumbersArePinned(t *testing.T) {
	tests := []struct {
		msg   proto.Message
		field protoreflect.Name
		want  protoreflect.FieldNumber
	}{
		{&workerv1.Offer{}, "offer_id", 1},
		{&workerv1.Offer{}, "public_repo", 6},
		{&workerv1.Offer{}, "expires_at", 7},
		{&workerv1.Offer{}, "cache_mode", 8},
		{&workerv1.OpenSessionRequest{}, "accept_public", 5},
		{&workerv1.OpenSessionResponse{}, "rejected_runners", 6},
		{&workerv1.AcceptOfferResponse{}, "encoded_jit_config", 3},
		{&workerv1.ReportRunnerRequest{}, "preempted", 6},
		{&workerv1.EnrollResponse{}, "owner_scopes", 3},
		{&workerv1.HeartbeatRequest{}, "accept_public", 4},
	}
	for _, tt := range tests {
		desc := tt.msg.ProtoReflect().Descriptor()
		fd := desc.Fields().ByName(tt.field)
		if fd == nil {
			t.Errorf("%s has no field %s", desc.FullName(), tt.field)
			continue
		}
		if fd.Number() != tt.want {
			t.Errorf("%s.%s = %d, want %d", desc.FullName(), tt.field, fd.Number(), tt.want)
		}
	}
}
