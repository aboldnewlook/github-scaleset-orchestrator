package ghsim

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

func TestSimJITRoundTripsAndPassesWorkerVerification(t *testing.T) {
	enc := EncodeJIT("https://github.com/o/a", "http://sim.test:9300", "gso-alpha-a-abc123", "tok-123")
	sj, ok, err := DecodeSimJIT(enc)
	if err != nil || !ok {
		t.Fatalf("DecodeSimJIT: ok=%v err=%v", ok, err)
	}
	want := SimJIT{GitHubURL: "https://github.com/o/a", ServerURL: "http://sim.test:9300", RunnerName: "gso-alpha-a-abc123", Token: "tok-123"}
	if sj != want {
		t.Fatalf("decoded %+v, want %+v", sj, want)
	}
	info, err := worker.VerifyJIT(enc, worker.OfferCheck{Repo: "o/a", Allowlist: []string{"o/*"}, TrustedHosts: []string{"github.com", "sim.test"}})
	if err != nil || info.Repo != "o/a" {
		t.Fatalf("worker.VerifyJIT = %+v, %v; a worker must accept simulator JIT configs", info, err)
	}
}

func TestDecodeSimJITIgnoresRealConfigsAndNeverEchoesInput(t *testing.T) {
	runnerFile := base64.StdEncoding.EncodeToString([]byte(`{"agentName":"r","gitHubUrl":"https://github.com/o/a"}`))
	realJIT := base64.StdEncoding.EncodeToString([]byte(`{".runner":"` + runnerFile + `"}`))
	if _, ok, err := DecodeSimJIT(realJIT); ok || err != nil {
		t.Fatalf("a real JIT config: ok=%v err=%v; want false, nil", ok, err)
	}
	_, ok, err := DecodeSimJIT("not-base64!!secret-material")
	if ok || err == nil {
		t.Fatal("garbage decoded")
	}
	if strings.Contains(err.Error(), "secret-material") {
		t.Fatalf("the error echoes its input: %v", err)
	}
}
