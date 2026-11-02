package ghsim

import (
	"crypto/subtle"
	"fmt"

	"github.com/actions/scaleset"
)

// mint registers a runner on the scale set and returns its JIT config.
func (m *Model) mint(repo string, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	if setting == nil || setting.Name == "" {
		return nil, fmt.Errorf("ghsim: a runner needs a name: %w", scaleset.BadRequestError)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ss, ok := m.scaleSets[scaleSetID]
	if !ok || !sameRepo(ss.repo, repo) {
		return nil, fmt.Errorf("ghsim: scale set %d: %w", scaleSetID, scaleset.NotFoundError)
	}
	if prev := m.runners[setting.Name]; prev != nil && !prev.removed && !prev.finished {
		return nil, fmt.Errorf("ghsim: runner %q: %w", setting.Name, scaleset.RunnerExistsError)
	}
	m.nextRunnerID++
	r := &runnerState{id: m.nextRunnerID, name: setting.Name, token: randomHex(16), repo: ss.repo, scaleSetID: scaleSetID}
	m.runners[r.name] = r
	m.allRunners = append(m.allRunners, r)
	m.repoLocked(repo).mints++
	m.notifyLocked()
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           &scaleset.RunnerReference{ID: int(r.id), Name: r.name, RunnerScaleSetID: scaleSetID},
		EncodedJITConfig: EncodeJIT("https://github.com/"+ss.repo, m.opts.ServerURL, r.name, r.token),
	}, nil
}

// runnerByName returns a registered runner of repo, or nil.
func (m *Model) runnerByName(repo, name string) *scaleset.RunnerReference {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.runners[name]
	if r == nil || !sameRepo(r.repo, repo) || r.removed || r.finished {
		return nil
	}
	return &scaleset.RunnerReference{ID: int(r.id), Name: r.name, RunnerScaleSetID: r.scaleSetID}
}

func (m *Model) removeRunner(repo string, runnerID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.allRunners {
		if r.id != runnerID || !sameRepo(r.repo, repo) || r.removed || r.finished {
			continue
		}
		if r.jobID != 0 {
			if j := m.jobLocked(r.jobID); j != nil && j.State == JobRunning {
				return fmt.Errorf("ghsim: runner %d: %w", runnerID, scaleset.JobStillRunningError)
			}
		}
		r.removed = true
		rs := m.repoLocked(repo)
		rs.removed = append(rs.removed, runnerID)
		m.notifyLocked()
		return nil
	}
	return fmt.Errorf("ghsim: runner %d: %w", runnerID, scaleset.RunnerNotFoundError)
}

// runnerLocked authenticates a runner. Finished runners are allowed only for
// status and completion calls, so a repeated completion stays harmless.
func (m *Model) runnerLocked(name, token string, allowFinished bool) (*runnerState, error) {
	r := m.runners[name]
	if r == nil || subtle.ConstantTimeCompare([]byte(r.token), []byte(token)) != 1 {
		return nil, ErrUnknownRunner
	}
	if r.removed {
		return nil, ErrRunnerRemoved
	}
	if r.finished && !allowFinished {
		return nil, ErrUnknownRunner
	}
	return r, nil
}
