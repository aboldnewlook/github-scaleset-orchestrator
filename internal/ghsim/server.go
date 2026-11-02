package ghsim

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/timing"
)

type apiError struct {
	Kind    string `json:"error"`
	Message string `json:"message"`
}

type lookupRequest struct {
	Name string `json:"name"`
}

type jitRequest struct {
	ScaleSetID int    `json:"scale_set_id"`
	Name       string `json:"name"`
}

type sessionRequest struct {
	ScaleSetID int    `json:"scale_set_id"`
	Owner      string `json:"owner"`
}

type acquireBody struct {
	IDs []int64 `json:"ids"`
}

type runnerRequest struct {
	Name   string `json:"name"`
	Token  string `json:"token"`
	JobID  int64  `json:"job_id,omitempty"`
	Result string `json:"result,omitempty"`
}

type statusResponse struct {
	Cancelled bool `json:"cancelled"`
}

type cancelRequest struct {
	Repo  string `json:"repo"`
	RunID int64  `json:"run_id"`
}

type scaleSetEnvelope struct {
	ScaleSet *scaleset.RunnerScaleSet `json:"scale_set"`
}

type runnerEnvelope struct {
	Runner *scaleset.RunnerReference `json:"runner"`
}

// InjectRequest is the body of POST /admin/jobs.
type InjectRequest struct {
	JobSpec
	Count int `json:"count,omitempty"`
}

type server struct {
	m         *Model
	logger    *slog.Logger
	claimWait time.Duration
	key       []byte // signs the wire-protocol tokens (see github.go)
}

// NewServer exposes the model over plain HTTP: the router API (what
// NewRemoteFactory calls), the runner API (what fake runners call) and the
// admin API (job injection). It has no authentication; never expose it.
func NewServer(m *Model, logger *slog.Logger) http.Handler {
	return newServer(m, logger, timing.Scale(25*time.Second))
}

func newServer(m *Model, logger *slog.Logger, claimWait time.Duration) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("ghsim: crypto/rand: " + err.Error())
	}
	s := &server{m: m, logger: logger, claimWait: claimWait, key: key}
	const repo = "/api/repos/{owner}/{name}"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET "+repo, s.repoInfo)
	mux.HandleFunc("POST "+repo+"/scalesets/lookup", s.lookupScaleSet)
	mux.HandleFunc("POST "+repo+"/scalesets", s.createScaleSet)
	mux.HandleFunc("PUT "+repo+"/scalesets/{id}", s.updateScaleSet)
	mux.HandleFunc("GET "+repo+"/scalesets", s.listScaleSets)
	mux.HandleFunc("POST "+repo+"/jit", s.mint)
	mux.HandleFunc("GET "+repo+"/runners", s.runnerByName)
	mux.HandleFunc("DELETE "+repo+"/runners/{id}", s.removeRunner)
	mux.HandleFunc("POST "+repo+"/sessions", s.openSession)
	mux.HandleFunc("DELETE "+repo+"/sessions/{sid}", s.closeSession)
	mux.HandleFunc("GET "+repo+"/sessions/{sid}/message", s.getMessage)
	mux.HandleFunc("DELETE "+repo+"/sessions/{sid}/messages/{mid}", s.deleteMessage)
	mux.HandleFunc("POST "+repo+"/sessions/{sid}/acquire", s.acquire)
	mux.HandleFunc("GET "+repo+"/runs/{id}", s.workflowRun)
	mux.HandleFunc("POST "+repo+"/runs/{id}/cancel", s.cancelRun)
	mux.HandleFunc("POST /runner/claim", s.claim)
	mux.HandleFunc("POST /runner/status", s.status)
	mux.HandleFunc("POST /runner/complete", s.complete)
	mux.HandleFunc("POST /admin/jobs", s.injectJobs)
	mux.HandleFunc("GET /admin/jobs", s.listJobs)
	mux.HandleFunc("GET /admin/runners", s.listRunners)
	mux.HandleFunc("POST /admin/runs/cancel", s.adminCancel)
	s.registerGitHub(mux)
	return s.logRequests(mux)
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.logger.Debug("sim request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func repoOf(r *http.Request) string { return r.PathValue("owner") + "/" + r.PathValue("name") }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func classify(err error) (string, int) {
	switch {
	case errors.Is(err, scaleset.ConflictError):
		return "conflict", http.StatusConflict
	case errors.Is(err, scaleset.RunnerExistsError):
		return "runner_exists", http.StatusConflict
	case errors.Is(err, scaleset.JobStillRunningError):
		return "job_still_running", http.StatusConflict
	case errors.Is(err, scaleset.RunnerNotFoundError):
		return "runner_not_found", http.StatusNotFound
	case errors.Is(err, scaleset.NotFoundError):
		return "not_found", http.StatusNotFound
	case errors.Is(err, scaleset.MessageQueueTokenExpiredError):
		return "session_expired", http.StatusGone
	case errors.Is(err, scaleset.BadRequestError):
		return "bad_request", http.StatusBadRequest
	case errors.Is(err, ErrRunnerRemoved):
		return "runner_removed", http.StatusGone
	case errors.Is(err, ErrUnknownRunner):
		return "unknown_runner", http.StatusUnauthorized
	case errors.Is(err, ErrUnknownJob):
		return "unknown_job", http.StatusNotFound
	default:
		return "internal", http.StatusInternalServerError
	}
}

func writeErr(w http.ResponseWriter, err error) {
	kind, status := classify(err)
	writeJSON(w, status, apiError{Kind: kind, Message: err.Error()})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Kind: "bad_request", Message: "invalid JSON body"})
		return false
	}
	return true
}

func pathInt(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Kind: "bad_request", Message: "invalid " + key})
		return 0, false
	}
	return v, true
}

func pathSession(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	sid, err := uuid.Parse(r.PathValue("sid"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Kind: "bad_request", Message: "invalid session id"})
		return uuid.Nil, false
	}
	return sid, true
}

func (s *server) repoInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.m.repoInfo(repoOf(r)))
}

func (s *server) lookupScaleSet(w http.ResponseWriter, r *http.Request) {
	var req lookupRequest
	if !readJSON(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, scaleSetEnvelope{ScaleSet: s.m.getScaleSet(repoOf(r), req.Name)})
}

func (s *server) createScaleSet(w http.ResponseWriter, r *http.Request) {
	var in scaleset.RunnerScaleSet
	if !readJSON(w, r, &in) {
		return
	}
	out, err := s.m.createScaleSet(repoOf(r), &in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) updateScaleSet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var in scaleset.RunnerScaleSet
	if !readJSON(w, r, &in) {
		return
	}
	out, err := s.m.updateScaleSet(repoOf(r), int(id), &in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) listScaleSets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.m.listScaleSets(repoOf(r)))
}

func (s *server) mint(w http.ResponseWriter, r *http.Request) {
	var req jitRequest
	if !readJSON(w, r, &req) {
		return
	}
	out, err := s.m.mint(repoOf(r), &scaleset.RunnerScaleSetJitRunnerSetting{Name: req.Name}, req.ScaleSetID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) runnerByName(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, runnerEnvelope{Runner: s.m.runnerByName(repoOf(r), r.URL.Query().Get("name"))})
}

func (s *server) removeRunner(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	if err := s.m.removeRunner(repoOf(r), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) openSession(w http.ResponseWriter, r *http.Request) {
	var req sessionRequest
	if !readJSON(w, r, &req) {
		return
	}
	sess, err := s.m.openSession(repoOf(r), req.ScaleSetID, req.Owner)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *server) closeSession(w http.ResponseWriter, r *http.Request) {
	sid, ok := pathSession(w, r)
	if !ok {
		return
	}
	_ = s.m.closeSession(repoOf(r), sid)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getMessage(w http.ResponseWriter, r *http.Request) {
	sid, ok := pathSession(w, r)
	if !ok {
		return
	}
	capacity, _ := strconv.Atoi(r.URL.Query().Get("capacity"))
	msg, err := s.m.getMessage(r.Context(), repoOf(r), sid, capacity)
	switch {
	case err != nil && r.Context().Err() != nil:
		return // the router went away mid-poll
	case err != nil:
		writeErr(w, err)
	case msg == nil:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, msg)
	}
}

func (s *server) deleteMessage(w http.ResponseWriter, r *http.Request) {
	sid, ok := pathSession(w, r)
	if !ok {
		return
	}
	mid, ok := pathInt(w, r, "mid")
	if !ok {
		return
	}
	if err := s.m.deleteMessage(repoOf(r), sid, int(mid)); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) acquire(w http.ResponseWriter, r *http.Request) {
	sid, ok := pathSession(w, r)
	if !ok {
		return
	}
	var body acquireBody
	if !readJSON(w, r, &body) {
		return
	}
	ids, err := s.m.acquireJobs(repoOf(r), sid, body.IDs)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acquireBody{IDs: ids})
}

func (s *server) workflowRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	run, err := s.m.workflowRun(repoOf(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	if err := s.m.CancelRun(repoOf(r), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) claim(w http.ResponseWriter, r *http.Request) {
	var req runnerRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.claimWait)
	defer cancel()
	c, err := s.m.ClaimJob(ctx, req.Name, req.Token)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, c)
	case r.Context().Err() != nil:
		return
	case errors.Is(err, context.DeadlineExceeded):
		w.WriteHeader(http.StatusNoContent) // nothing yet; the runner polls again
	default:
		writeErr(w, err)
	}
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	var req runnerRequest
	if !readJSON(w, r, &req) {
		return
	}
	cancelled, err := s.m.JobStatus(req.Name, req.Token, req.JobID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{Cancelled: cancelled})
}

func (s *server) complete(w http.ResponseWriter, r *http.Request) {
	var req runnerRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.m.CompleteJob(req.Name, req.Token, req.JobID, req.Result); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) injectJobs(w http.ResponseWriter, r *http.Request) {
	var req InjectRequest
	if !readJSON(w, r, &req) {
		return
	}
	count := req.Count
	if count <= 0 {
		count = 1
	}
	if count > 100 {
		writeJSON(w, http.StatusBadRequest, apiError{Kind: "bad_request", Message: "count must be 1-100"})
		return
	}
	jobs := make([]JobState, 0, count)
	for range count {
		st, err := s.m.InjectJob(req.JobSpec)
		if err != nil {
			writeErr(w, err)
			return
		}
		jobs = append(jobs, st)
	}
	s.logger.Info("injected simulated jobs", "repo", req.Repo, "count", count, "labels", req.Labels)
	writeJSON(w, http.StatusOK, jobs)
}

func (s *server) listJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.m.Jobs())
}

func (s *server) listRunners(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.m.AllRunners())
}

func (s *server) adminCancel(w http.ResponseWriter, r *http.Request) {
	var req cancelRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.m.CancelRun(req.Repo, req.RunID); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
