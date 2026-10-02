package git_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTimeoutBoundsDetachedHookPipes(t *testing.T) {
	t.Parallel()

	_, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is required to exercise a detached hook")
	}

	for _, ending := range []string{"wait", "exit 0"} {
		t.Run(ending, func(t *testing.T) {
			t.Parallel()
			repository := newRepository(t)
			writeRepositoryFile(t, repository, testFile, "pending")
			pidFile := filepath.Join(repository.Path, ".git", "escaped-pid")

			t.Cleanup(func() { stopDetachedHook(t, pidFile) })

			hook := "#!/bin/sh\nsetsid sleep 8 &\nprintf '%s' \"$!\" > .git/escaped-pid\n" + ending + "\n"
			//nolint:gosec // Executable fixture stays inside this private repository.
			err := os.WriteFile(filepath.Join(repository.Path, ".git", "hooks", "pre-commit"), []byte(hook), 0o700)
			if err != nil {
				t.Fatal(err)
			}

			repository.Timeout = time.Second
			started := time.Now()

			_, err = repository.CommitPaths(t.Context(), "detached hook", []string{testFile})
			if err == nil {
				t.Fatal("detached pipe owner did not fail the command")
			}

			if time.Since(started) > 4*time.Second {
				t.Fatal("Wait exceeded timeout while a detached descendant held the pipes")
			}
		})
	}
}

func stopDetachedHook(t *testing.T, path string) {
	t.Helper()
	//nolint:gosec // Read the PID written by the hook in this test's private repository.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Error("hook was not reached", err)

		return
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Error(err)

		return
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		t.Error(err)

		return
	}

	err = process.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Error(err)
	}

	err = process.Release()
	if err != nil {
		t.Error(err)
	}
}
