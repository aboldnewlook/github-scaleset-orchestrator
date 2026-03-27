package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Worker manages a single ephemeral runner subprocess.
type Worker struct {
	runnerDir string
	logger    *slog.Logger
}

func NewWorker(runnerDir string, logger *slog.Logger) *Worker {
	return &Worker{runnerDir: runnerDir, logger: logger}
}

// Run creates a temporary working directory, copies the runner binary,
// and executes a single job using the provided JIT config.
// It blocks until the runner process exits and cleans up the temp directory.
func (w *Worker) Run(ctx context.Context, name string, jitConfig string) error {
	workDir, err := os.MkdirTemp("", "gso-"+name+"-*")
	if err != nil {
		return fmt.Errorf("creating work dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	w.logger.Info("preparing runner", "name", name, "workdir", workDir)

	if err := copyDir(w.runnerDir, workDir); err != nil {
		return fmt.Errorf("copying runner binaries: %w", err)
	}

	runScript := filepath.Join(workDir, "run.sh")
	if runtime.GOOS == "windows" {
		runScript = filepath.Join(workDir, "run.cmd")
	}

	cmd := exec.CommandContext(ctx, runScript)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("ACTIONS_RUNNER_INPUT_JITCONFIG=%s", jitConfig),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	w.logger.Info("starting runner", "name", name)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runner process exited: %w", err)
	}

	w.logger.Info("runner completed", "name", name)
	return nil
}

// copyDir copies the contents of src into dst using hard links where possible,
// falling back to regular copies.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}

		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, dstPath)
		}

		// Try hard link first (same filesystem, saves disk space)
		if err := os.Link(path, dstPath); err == nil {
			return nil
		}

		// Fall back to copy
		return copyFile(path, dstPath, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = out.ReadFrom(in)
	return err
}
