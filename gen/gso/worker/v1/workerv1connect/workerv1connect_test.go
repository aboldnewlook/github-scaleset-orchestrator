package workerv1connect_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1/workerv1connect"
)

// internal/fleet implements this interface (task B6).
var _ workerv1connect.WorkerServiceHandler = workerv1connect.UnimplementedWorkerServiceHandler{}

func TestUnimplementedHandlerAnswersUnimplemented(t *testing.T) {
	path, handler := workerv1connect.NewWorkerServiceHandler(workerv1connect.UnimplementedWorkerServiceHandler{})
	if path != "/gso.worker.v1.WorkerService/" {
		t.Fatalf("handler path = %q", path)
	}
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := workerv1connect.NewWorkerServiceClient(srv.Client(), srv.URL)

	_, err := client.Heartbeat(context.Background(), connect.NewRequest(&workerv1.HeartbeatRequest{SessionId: "s"}))
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("Heartbeat code = %v (err %v), want unimplemented", got, err)
	}

	stream, err := client.Subscribe(context.Background(), connect.NewRequest(&workerv1.SubscribeRequest{SessionId: "s"}))
	if err == nil {
		defer func() { _ = stream.Close() }()
		if stream.Receive() {
			t.Fatal("an unimplemented server sent a directive")
		}
		err = stream.Err()
	}
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("Subscribe code = %v (err %v), want unimplemented", got, err)
	}
}
