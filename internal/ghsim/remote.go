package ghsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
)

var sentinels = map[string]error{
	"conflict":          scaleset.ConflictError,
	"runner_exists":     scaleset.RunnerExistsError,
	"job_still_running": scaleset.JobStillRunningError,
	"runner_not_found":  scaleset.RunnerNotFoundError,
	"not_found":         scaleset.NotFoundError,
	"session_expired":   scaleset.MessageQueueTokenExpiredError,
	"bad_request":       scaleset.BadRequestError,
	"runner_removed":    ErrRunnerRemoved,
	"unknown_runner":    ErrUnknownRunner,
	"unknown_job":       ErrUnknownJob,
}

func errorFromAPI(status int, body []byte) error {
	var ae apiError
	if err := json.Unmarshal(body, &ae); err != nil || ae.Kind == "" {
		return fmt.Errorf("ghsim: HTTP %d", status)
	}
	if s, ok := sentinels[ae.Kind]; ok {
		return fmt.Errorf("ghsim: %s: %w", ae.Message, s)
	}
	return fmt.Errorf("ghsim: %s (%s)", ae.Message, ae.Kind)
}

type httpDoer struct {
	base   string
	client *http.Client
}

// do sends in as JSON and decodes a 200 into out. It returns the status code;
// a 4xx/5xx becomes an error wrapping the matching sentinel.
func (h httpDoer) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("ghsim: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return resp.StatusCode, errorFromAPI(resp.StatusCode, b)
	}
	if resp.StatusCode != http.StatusOK || out == nil {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("ghsim: decoding %s %s: %w", method, path, err)
	}
	return resp.StatusCode, nil
}

// NewRemoteFactory returns a ghapi.Factory whose clients talk to a simulator
// served by NewServer at baseURL.
func NewRemoteFactory(baseURL string, httpClient *http.Client) ghapi.Factory {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Minute}
	}
	h := httpDoer{base: strings.TrimRight(baseURL, "/"), client: httpClient}
	return func(_ context.Context, repo config.Repo) (ghapi.Client, error) {
		owner, name, ok := strings.Cut(repo.Name, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("ghsim: repo %q must be owner/name", repo.Name)
		}
		return &remoteClient{h: h, prefix: "/api/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)}, nil
	}
}

type remoteClient struct {
	h      httpDoer
	prefix string
}

var _ ghapi.Client = (*remoteClient)(nil)

func (c *remoteClient) GetRunnerScaleSet(ctx context.Context, _ int, name string) (*scaleset.RunnerScaleSet, error) {
	var env scaleSetEnvelope
	if _, err := c.h.do(ctx, http.MethodPost, c.prefix+"/scalesets/lookup", lookupRequest{Name: name}, &env); err != nil {
		return nil, err
	}
	return env.ScaleSet, nil
}

func (c *remoteClient) CreateRunnerScaleSet(ctx context.Context, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	var out scaleset.RunnerScaleSet
	if _, err := c.h.do(ctx, http.MethodPost, c.prefix+"/scalesets", ss, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *remoteClient) UpdateRunnerScaleSet(ctx context.Context, id int, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	var out scaleset.RunnerScaleSet
	if _, err := c.h.do(ctx, http.MethodPut, c.prefix+"/scalesets/"+strconv.Itoa(id), ss, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *remoteClient) ListRunnerScaleSets(ctx context.Context, _ int) ([]scaleset.RunnerScaleSet, error) {
	var out []scaleset.RunnerScaleSet
	if _, err := c.h.do(ctx, http.MethodGet, c.prefix+"/scalesets", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *remoteClient) GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	if setting == nil {
		return nil, fmt.Errorf("ghsim: a runner needs a name: %w", scaleset.BadRequestError)
	}
	var out scaleset.RunnerScaleSetJitRunnerConfig
	if _, err := c.h.do(ctx, http.MethodPost, c.prefix+"/jit", jitRequest{ScaleSetID: scaleSetID, Name: setting.Name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *remoteClient) GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error) {
	var env runnerEnvelope
	if _, err := c.h.do(ctx, http.MethodGet, c.prefix+"/runners?name="+url.QueryEscape(name), nil, &env); err != nil {
		return nil, err
	}
	return env.Runner, nil
}

func (c *remoteClient) RemoveRunner(ctx context.Context, runnerID int64) error {
	_, err := c.h.do(ctx, http.MethodDelete, c.prefix+"/runners/"+strconv.FormatInt(runnerID, 10), nil, nil)
	return err
}

func (c *remoteClient) SetSystemInfo(scaleset.SystemInfo) {}

func (c *remoteClient) OpenSession(ctx context.Context, scaleSetID int, owner string) (ghapi.Session, error) {
	var info scaleset.RunnerScaleSetSession
	if _, err := c.h.do(ctx, http.MethodPost, c.prefix+"/sessions", sessionRequest{ScaleSetID: scaleSetID, Owner: owner}, &info); err != nil {
		return nil, err
	}
	return &remoteSession{c: c, info: info}, nil
}

func (c *remoteClient) Repo(ctx context.Context) (ghapi.RepoInfo, error) {
	var out ghapi.RepoInfo
	_, err := c.h.do(ctx, http.MethodGet, c.prefix, nil, &out)
	return out, err
}

func (c *remoteClient) WorkflowRun(ctx context.Context, runID int64) (ghapi.WorkflowRun, error) {
	var out ghapi.WorkflowRun
	_, err := c.h.do(ctx, http.MethodGet, c.prefix+"/runs/"+strconv.FormatInt(runID, 10), nil, &out)
	return out, err
}

func (c *remoteClient) CancelWorkflowRun(ctx context.Context, runID int64) error {
	_, err := c.h.do(ctx, http.MethodPost, c.prefix+"/runs/"+strconv.FormatInt(runID, 10)+"/cancel", nil, nil)
	return err
}

type remoteSession struct {
	c    *remoteClient
	info scaleset.RunnerScaleSetSession
}

var _ ghapi.Session = (*remoteSession)(nil)

func (s *remoteSession) path() string { return s.c.prefix + "/sessions/" + s.info.SessionID.String() }

func (s *remoteSession) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	var msg scaleset.RunnerScaleSetMessage
	q := "?last=" + strconv.Itoa(lastMessageID) + "&capacity=" + strconv.Itoa(maxCapacity)
	status, err := s.c.h.do(ctx, http.MethodGet, s.path()+"/message"+q, nil, &msg)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &msg, nil
}

func (s *remoteSession) DeleteMessage(ctx context.Context, messageID int) error {
	_, err := s.c.h.do(ctx, http.MethodDelete, s.path()+"/messages/"+strconv.Itoa(messageID), nil, nil)
	return err
}

func (s *remoteSession) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	var out acquireBody
	if _, err := s.c.h.do(ctx, http.MethodPost, s.path()+"/acquire", acquireBody{IDs: requestIDs}, &out); err != nil {
		return nil, err
	}
	return out.IDs, nil
}

func (s *remoteSession) Session() scaleset.RunnerScaleSetSession { return s.info }

func (s *remoteSession) Close(ctx context.Context) error {
	_, err := s.c.h.do(ctx, http.MethodDelete, s.path(), nil, nil)
	if errors.Is(err, scaleset.MessageQueueTokenExpiredError) {
		return nil
	}
	return err
}
