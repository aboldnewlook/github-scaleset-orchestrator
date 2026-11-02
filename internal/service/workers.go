package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
)

// WorkerAdmin is the router's worker administration (spec §4.4). The runtime
// serves it only on the local unix socket (spec §8.1, SEC-F8).
type WorkerAdmin interface {
	AddWorker(ctx context.Context, p control.WorkersAddParams) (control.WorkersAddResult, error)
	UpdateWorker(ctx context.Context, p control.WorkersUpdateParams) error
	ListWorkers() []control.WorkerLiveStatus
	DrainWorker(name, reason string) error
	RevokeWorker(ctx context.Context, name string) error
}

const errUnixOnly = "workers_* methods are only available on the local unix socket"

func isWorkersMethod(method string) bool {
	switch method {
	case control.MethodWorkersAdd, control.MethodWorkersUpdate, control.MethodWorkersList,
		control.MethodWorkersDrain, control.MethodWorkersRevoke:
		return true
	}
	return false
}

// handleWorkers serves the workers_* methods. Responses never echo params:
// the join string in a workers_add result carries an enrollment code.
func (r *Runtime) handleWorkers(ctx context.Context, req control.Request) control.Response {
	if !control.IsUnixSocket(ctx) {
		r.logger.Warn("refused worker administration outside the unix socket", "method", req.Method)
		return control.Response{Error: errUnixOnly}
	}
	admin, ok := r.orch.(WorkerAdmin)
	if !ok {
		return control.Response{Error: "this daemon does not manage workers"}
	}
	switch req.Method {
	case control.MethodWorkersAdd:
		var p control.WorkersAddParams
		if err := decodeParams(req, &p); err != nil {
			return control.Response{Error: err.Error()}
		}
		res, err := admin.AddWorker(ctx, p)
		if err != nil {
			return control.Response{Error: err.Error()}
		}
		return resultResponse(res)
	case control.MethodWorkersUpdate:
		var p control.WorkersUpdateParams
		if err := decodeParams(req, &p); err != nil {
			return control.Response{Error: err.Error()}
		}
		return errResponse(admin.UpdateWorker(ctx, p))
	case control.MethodWorkersList:
		return resultResponse(admin.ListWorkers())
	case control.MethodWorkersDrain:
		var p control.WorkerNameParams
		if err := decodeParams(req, &p); err != nil {
			return control.Response{Error: err.Error()}
		}
		return errResponse(admin.DrainWorker(p.Name, p.Reason))
	case control.MethodWorkersRevoke:
		var p control.WorkerNameParams
		if err := decodeParams(req, &p); err != nil {
			return control.Response{Error: err.Error()}
		}
		return errResponse(admin.RevokeWorker(ctx, p.Name))
	}
	return control.Response{Error: fmt.Sprintf("unknown method: %s", req.Method)}
}

func decodeParams(req control.Request, v any) error {
	if len(req.Params) == 0 {
		return errors.New("invalid params: missing")
	}
	if err := json.Unmarshal(req.Params, v); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}

func errResponse(err error) control.Response {
	if err != nil {
		return control.Response{Error: err.Error()}
	}
	return control.Response{}
}

func resultResponse(v any) control.Response {
	data, err := json.Marshal(v)
	if err != nil {
		return control.Response{Error: err.Error()}
	}
	return control.Response{Result: data}
}
