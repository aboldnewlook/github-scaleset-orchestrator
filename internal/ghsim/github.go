package ghsim

// This file speaks the wire protocol of GitHub and the Actions service as
// actions/scaleset v0.5 uses it, so the unmodified scaleset client (and
// listener) can run against the simulator. The client treats any host other
// than github.com as GitHub Enterprise Server, which is how it finds us:
//
//	POST /api/v3/repos/{o}/{r}/actions/runners/registration-token  -> 201
//	POST /api/v3/actions/runner-registration {url}                 -> {url, token(JWT)}
//	     ... then the Actions service under the returned url:
//	/_actions/{o}/{r}/_apis/runtime/runnerscalesets[/{id}[/sessions|messages|...]]
//	/_actions/{o}/{r}/_apis/distributedtask/pools/0/agents[/{id}]
//
// The repository travels in the service URL path. Nothing is authenticated;
// the bearer values only have to look right. Message-queue tokens are carried
// in the Authorization header and never in a URL.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

const (
	serviceRoot   = "/_actions/{owner}/{name}/_apis"
	setsPath      = serviceRoot + "/runtime/runnerscalesets"
	agentsPath    = serviceRoot + "/distributedtask/pools/0/agents"
	restPath      = "/api/v3/repos/{owner}/{name}"
	adminTokenTTL = time.Hour
)

func (s *server) registerGitHub(mux *http.ServeMux) {
	mux.HandleFunc("POST "+restPath+"/actions/runners/registration-token", s.registrationToken)
	mux.HandleFunc("POST /api/v3/actions/runner-registration", s.runnerRegistration)
	mux.HandleFunc("GET "+restPath, s.ghRepo)
	mux.HandleFunc("GET "+restPath+"/actions/runs/{id}", s.ghRun)
	mux.HandleFunc("POST "+restPath+"/actions/runs/{id}/cancel", s.ghCancelRun)

	mux.HandleFunc("GET "+setsPath, s.svcListScaleSets)
	mux.HandleFunc("POST "+setsPath, s.svcCreateScaleSet)
	mux.HandleFunc("GET "+setsPath+"/{id}", s.svcGetScaleSet)
	mux.HandleFunc("PATCH "+setsPath+"/{id}", s.svcUpdateScaleSet)
	mux.HandleFunc("DELETE "+setsPath+"/{id}", s.svcDeleteScaleSet)
	mux.HandleFunc("POST "+setsPath+"/{id}/sessions", s.svcOpenSession)
	mux.HandleFunc("PATCH "+setsPath+"/{id}/sessions/{sid}", s.svcRefreshSession)
	mux.HandleFunc("DELETE "+setsPath+"/{id}/sessions/{sid}", s.svcCloseSession)
	mux.HandleFunc("GET "+setsPath+"/{id}/messages", s.svcGetMessage)
	mux.HandleFunc("DELETE "+setsPath+"/{id}/messages/{mid}", s.svcDeleteMessage)
	mux.HandleFunc("POST "+setsPath+"/{id}/acquirejobs", s.svcAcquire)
	mux.HandleFunc("POST "+setsPath+"/{id}/generatejitconfig", s.svcMint)
	mux.HandleFunc("GET "+agentsPath, s.svcRunnerByName)
	mux.HandleFunc("DELETE "+agentsPath+"/{rid}", s.svcRemoveRunner)
}

// ---- errors ----------------------------------------------------------------

// writeSvcErr answers in the shape scaleset's newRequestResponseError parses:
// a status plus {"typeName", "message"}.
func (s *server) writeSvcErr(w http.ResponseWriter, err error) {
	type body struct {
		TypeName string `json:"typeName"`
		Message  string `json:"message"`
	}
	var status int
	var typ string
	switch {
	case errors.Is(err, scaleset.RunnerExistsError):
		status, typ = http.StatusConflict, "AgentExistsException"
	case errors.Is(err, scaleset.ConflictError):
		status, typ = http.StatusConflict, "TaskAgentSessionConflictException"
	case errors.Is(err, scaleset.JobStillRunningError):
		status, typ = http.StatusBadRequest, "JobStillRunningException"
	case errors.Is(err, scaleset.RunnerNotFoundError):
		status, typ = http.StatusNotFound, "AgentNotFoundException"
	case errors.Is(err, scaleset.NotFoundError):
		status, typ = http.StatusNotFound, "NotFoundException"
	case errors.Is(err, scaleset.MessageQueueTokenExpiredError):
		status, typ = http.StatusUnauthorized, "MessageQueueTokenExpiredException"
	case errors.Is(err, scaleset.BadRequestError):
		status, typ = http.StatusBadRequest, "ArgumentException"
	default:
		s.logger.Error("sim request failed", "error", err.Error())
		status, typ = http.StatusInternalServerError, "InternalException"
	}
	writeJSON(w, status, body{TypeName: typ, Message: err.Error()})
}

func (s *server) writeRESTErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, scaleset.NotFoundError):
		status = http.StatusNotFound
	case errors.Is(err, scaleset.BadRequestError):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"message": err.Error()})
}

// ---- registration -----------------------------------------------------------

func (s *server) registrationToken(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      "sim-registration-token",
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
}

// adminJWT is an HS256 token whose only job is to carry an exp claim the
// scaleset client can read. The client never verifies it.
func (s *server) adminJWT() string {
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iss": "gso-sim", "exp": time.Now().Add(adminTokenTTL).Unix()})
	signing := head + "." + enc.EncodeToString(claims)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(signing))
	return signing + "." + enc.EncodeToString(mac.Sum(nil))
}

// serviceBase is the Actions service URL of one repository, as the caller reached us.
func serviceBase(r *http.Request, owner, name string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/_actions/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func (s *server) runnerRegistration(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	const badURL = "url must point at a repository: <sim>/owner/name"
	u, err := url.Parse(req.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": badURL})
		return
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": badURL})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": serviceBase(r, parts[0], parts[1]), "token": s.adminJWT()})
}

// ---- message-queue tokens ---------------------------------------------------

func (s *server) queueToken(sid uuid.UUID) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(sid.String()))
	return sid.String() + "." + hex.EncodeToString(mac.Sum(nil))
}

// sessionFromToken resolves the Authorization bearer to a session ID.
func (s *server) sessionFromToken(r *http.Request) (uuid.UUID, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	idPart, _, found := strings.Cut(tok, ".")
	if !ok || !found {
		return uuid.Nil, scaleset.MessageQueueTokenExpiredError
	}
	sid, err := uuid.Parse(idPart)
	if err != nil || subtle.ConstantTimeCompare([]byte(tok), []byte(s.queueToken(sid))) != 1 {
		return uuid.Nil, scaleset.MessageQueueTokenExpiredError
	}
	return sid, nil
}

// ---- REST -------------------------------------------------------------------

func (s *server) ghRepo(w http.ResponseWriter, r *http.Request) {
	info := s.m.repoInfo(repoOf(r))
	writeJSON(w, http.StatusOK, map[string]any{"full_name": info.FullName, "private": info.Private, "visibility": info.Visibility})
}

func (s *server) ghRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	run, err := s.m.workflowRun(repoOf(r), id)
	if err != nil {
		s.writeRESTErr(w, err)
		return
	}
	type repo struct {
		FullName string `json:"full_name"`
	}
	out := map[string]any{"id": run.ID, "event": run.Event, "repository": repo{run.RepoFullName}}
	if run.HeadRepoFullName != "" {
		out["head_repository"] = repo{run.HeadRepoFullName}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) ghCancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	if err := s.m.CancelRun(repoOf(r), id); err != nil {
		s.writeRESTErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ---- scale sets -------------------------------------------------------------

type svcSets struct {
	Count int                       `json:"count"`
	Value []scaleset.RunnerScaleSet `json:"value"`
}

func (s *server) svcListScaleSets(w http.ResponseWriter, r *http.Request) {
	all := s.m.listScaleSets(repoOf(r))
	if name := r.URL.Query().Get("name"); name != "" {
		var match []scaleset.RunnerScaleSet
		for _, ss := range all {
			if ss.Name == name {
				match = append(match, ss)
			}
		}
		all = match
	}
	writeJSON(w, http.StatusOK, svcSets{Count: len(all), Value: all})
}

func (s *server) svcCreateScaleSet(w http.ResponseWriter, r *http.Request) {
	var in scaleset.RunnerScaleSet
	if !readJSON(w, r, &in) {
		return
	}
	out, err := s.m.createScaleSet(repoOf(r), &in)
	if err != nil {
		s.writeSvcErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) svcGetScaleSet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	for _, ss := range s.m.listScaleSets(repoOf(r)) {
		if int64(ss.ID) == id {
			writeJSON(w, http.StatusOK, ss)
			return
		}
	}
	s.writeSvcErr(w, scaleset.NotFoundError)
}

func (s *server) svcUpdateScaleSet(w http.ResponseWriter, r *http.Request) {
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
		s.writeSvcErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) svcDeleteScaleSet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	if err := s.m.DeleteScaleSet(repoOf(r), int(id)); err != nil {
		s.writeSvcErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- sessions and messages --------------------------------------------------

// svcSession fills in the fields only the wire protocol has.
func (s *server) svcSession(r *http.Request, setID int, sess scaleset.RunnerScaleSetSession) scaleset.RunnerScaleSetSession {
	sess.MessageQueueURL = serviceBase(r, r.PathValue("owner"), r.PathValue("name")) +
		"/_apis/runtime/runnerscalesets/" + strconv.Itoa(setID) + "/messages?sessionId=" + sess.SessionID.String()
	sess.MessageQueueAccessToken = s.queueToken(sess.SessionID)
	return sess
}

func (s *server) svcOpenSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		OwnerName string `json:"ownerName"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	sess, err := s.m.openSession(repoOf(r), int(id), req.OwnerName)
	if err != nil {
		s.writeSvcErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.svcSession(r, int(id), sess))
}

func (s *server) svcRefreshSession(w http.ResponseWriter, r *http.Request) {
	sid, ok := pathSession(w, r)
	if !ok {
		return
	}
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	sess, err := s.m.sessionInfo(repoOf(r), sid)
	if err != nil {
		s.writeSvcErr(w, scaleset.NotFoundError)
		return
	}
	writeJSON(w, http.StatusOK, s.svcSession(r, int(id), sess))
}

func (s *server) svcCloseSession(w http.ResponseWriter, r *http.Request) {
	sid, ok := pathSession(w, r)
	if !ok {
		return
	}
	_ = s.m.closeSession(repoOf(r), sid)
	w.WriteHeader(http.StatusNoContent)
}

// sessionInfo returns the open session's current description.
func (m *Model) sessionInfo(repo string, sid uuid.UUID) (scaleset.RunnerScaleSetSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss, s, err := m.sessionLocked(repo, sid)
	if err != nil {
		return scaleset.RunnerScaleSetSession{}, err
	}
	set := m.publicSetLocked(ss)
	stats := m.statsLocked(ss)
	return scaleset.RunnerScaleSetSession{SessionID: sid, OwnerName: s.owner, RunnerScaleSet: &set, Statistics: &stats}, nil
}

func (s *server) svcGetMessage(w http.ResponseWriter, r *http.Request) {
	sid, err := s.sessionFromToken(r)
	if err != nil {
		s.writeSvcErr(w, err)
		return
	}
	capacity, _ := strconv.Atoi(r.Header.Get(scaleset.HeaderScaleSetMaxCapacity))
	msg, err := s.m.getMessage(r.Context(), repoOf(r), sid, capacity)
	switch {
	case err != nil && r.Context().Err() != nil:
		return // the caller went away mid-poll
	case err != nil:
		s.writeSvcErr(w, err)
	case msg == nil:
		w.WriteHeader(http.StatusAccepted)
	default:
		writeJSON(w, http.StatusOK, encodeMessage(msg))
	}
}

type wireMessage struct {
	MessageID   int                               `json:"messageId"`
	MessageType string                            `json:"messageType"`
	Body        string                            `json:"body"`
	Statistics  *scaleset.RunnerScaleSetStatistic `json:"statistics"`
}

// encodeMessage is the inverse of scaleset's parseRunnerScaleSetMessageResponse:
// the batched job messages travel as a JSON array inside the body string.
func encodeMessage(msg *scaleset.RunnerScaleSetMessage) wireMessage {
	batch := []any{}
	for _, v := range msg.JobAvailableMessages {
		c := *v
		c.MessageType = scaleset.MessageTypeJobAvailable
		batch = append(batch, c)
	}
	for _, v := range msg.JobAssignedMessages {
		c := *v
		c.MessageType = scaleset.MessageTypeJobAssigned
		batch = append(batch, c)
	}
	for _, v := range msg.JobStartedMessages {
		c := *v
		c.MessageType = scaleset.MessageTypeJobStarted
		batch = append(batch, c)
	}
	for _, v := range msg.JobCompletedMessages {
		c := *v
		c.MessageType = scaleset.MessageTypeJobCompleted
		batch = append(batch, c)
	}
	body, _ := json.Marshal(batch)
	return wireMessage{MessageID: msg.MessageID, MessageType: "RunnerScaleSetJobMessages", Body: string(body), Statistics: msg.Statistics}
}

func (s *server) svcDeleteMessage(w http.ResponseWriter, r *http.Request) {
	sid, err := s.sessionFromToken(r)
	if err != nil {
		s.writeSvcErr(w, err)
		return
	}
	mid, ok := pathInt(w, r, "mid")
	if !ok {
		return
	}
	if err := s.m.deleteMessage(repoOf(r), sid, int(mid)); err != nil {
		s.writeSvcErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) svcAcquire(w http.ResponseWriter, r *http.Request) {
	sid, err := s.sessionFromToken(r)
	if err != nil {
		s.writeSvcErr(w, err)
		return
	}
	var ids []int64
	if !readJSON(w, r, &ids) {
		return
	}
	got, err := s.m.acquireJobs(repoOf(r), sid, ids)
	if err != nil {
		s.writeSvcErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(got), "value": got})
}

// ---- runners ----------------------------------------------------------------

func (s *server) svcMint(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var in scaleset.RunnerScaleSetJitRunnerSetting
	if !readJSON(w, r, &in) {
		return
	}
	out, err := s.m.mint(repoOf(r), &in, int(id))
	if err != nil {
		s.writeSvcErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) svcRunnerByName(w http.ResponseWriter, r *http.Request) {
	list := []scaleset.RunnerReference{}
	if ref := s.m.runnerByName(repoOf(r), r.URL.Query().Get("agentName")); ref != nil {
		list = append(list, *ref)
	}
	writeJSON(w, http.StatusOK, scaleset.RunnerReferenceList{Count: len(list), RunnerReferences: list})
}

func (s *server) svcRemoveRunner(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "rid")
	if !ok {
		return
	}
	if err := s.m.removeRunner(repoOf(r), id); err != nil {
		s.writeSvcErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
