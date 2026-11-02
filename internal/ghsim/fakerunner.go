package ghsim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/timing"
)

// defaultClaimWait is how long the simulator holds a /runner/claim long-poll
// (before timing.Scale) before it answers 204.
const defaultClaimWait = 25 * time.Second

// fakeRunnerHTTPTimeout bounds one request from the fake runner. It must stay
// well above defaultClaimWait, or every idle claim poll would be aborted
// client-side (a unit test pins this).
const fakeRunnerHTTPTimeout = time.Minute

// fakeRunnerPoll is how often a working fake runner asks whether its job was cancelled.
var fakeRunnerPoll = func() time.Duration { return max(timing.Scale(time.Second), 50*time.Millisecond) }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RunFakeRunner stands in for the actions runner: it takes a simulator JIT
// config, claims a job from the simulator, works on it for the job's duration
// and reports it done. It returns nil when the job is reported, or when the
// runner was removed before any job arrived. It never logs the token.
func RunFakeRunner(ctx context.Context, encodedJIT string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	sj, ok, err := DecodeSimJIT(encodedJIT)
	if err != nil {
		return fmt.Errorf("fake runner: %w", err)
	}
	if !ok {
		return errors.New("fake runner: not a simulator JIT config")
	}
	if sj.ServerURL == "" {
		return errors.New("fake runner: the JIT config carries no simulator URL")
	}
	h := httpDoer{base: strings.TrimRight(sj.ServerURL, "/"), client: &http.Client{Timeout: fakeRunnerHTTPTimeout}}
	log := logger.With("runner", sj.RunnerName)
	log.Info("fake runner waiting for a job", "sim", h.base)

	claim, err := claimJob(ctx, h, sj, log)
	if err != nil {
		return err
	}
	if claim == nil {
		log.Info("runner removed before a job arrived")
		return nil
	}
	log.Info("fake runner working", "job", claim.JobID, "run", claim.RunID, "duration", claim.Duration)
	result := claim.Result
	if result == "" {
		result = "succeeded"
	}
	deadline := time.Now().Add(claim.Duration)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		if err := sleepCtx(ctx, min(left, fakeRunnerPoll())); err != nil {
			return err
		}
		var st statusResponse
		if _, err := h.do(ctx, http.MethodPost, "/runner/status", runnerRequest{Name: sj.RunnerName, Token: sj.Token, JobID: claim.JobID}, &st); err == nil && st.Cancelled {
			log.Info("job cancelled")
			result = "canceled"
			break
		}
	}
	return completeJob(ctx, h, sj, claim.JobID, result, log)
}

// claimJob long-polls until a job is bound to this runner. A nil claim with a
// nil error means the runner was removed first.
func claimJob(ctx context.Context, h httpDoer, sj SimJIT, log *slog.Logger) (*Claim, error) {
	for {
		var c Claim
		status, err := h.do(ctx, http.MethodPost, "/runner/claim", runnerRequest{Name: sj.RunnerName, Token: sj.Token}, &c)
		switch {
		case err == nil && status == http.StatusOK:
			return &c, nil
		case err == nil:
			continue // 204: nothing yet
		case errors.Is(err, ErrRunnerRemoved):
			return nil, nil
		case errors.Is(err, ErrUnknownRunner):
			return nil, errors.New("fake runner: the simulator does not know this runner")
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			log.Warn("claim failed; retrying", "error", err)
			if err := sleepCtx(ctx, fakeRunnerPoll()); err != nil {
				return nil, err
			}
		}
	}
}

func completeJob(ctx context.Context, h httpDoer, sj SimJIT, jobID int64, result string, log *slog.Logger) error {
	for {
		_, err := h.do(ctx, http.MethodPost, "/runner/complete", runnerRequest{Name: sj.RunnerName, Token: sj.Token, JobID: jobID, Result: result}, nil)
		switch {
		case err == nil:
			log.Info("job reported", "job", jobID, "result", result)
			return nil
		case errors.Is(err, ErrUnknownJob), errors.Is(err, ErrUnknownRunner), errors.Is(err, ErrRunnerRemoved):
			return fmt.Errorf("fake runner: the simulator refused the completion: %w", err)
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			log.Warn("completion failed; retrying", "error", err)
			if err := sleepCtx(ctx, fakeRunnerPoll()); err != nil {
				return err
			}
		}
	}
}
