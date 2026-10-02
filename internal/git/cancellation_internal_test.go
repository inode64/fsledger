package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestCancelFallsBackToDirectProcess(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// The child deliberately inherits this test's group, so kill(-childPID) returns ESRCH.
	command := exec.CommandContext(ctx, "sleep", "10")

	err := command.Start()
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	cancelErr := cancelGit(command)

	waitErr := command.Wait()
	if cancelErr != nil || waitErr == nil || time.Since(started) > time.Second {
		t.Fatal("direct child survived cancellation", cancelErr, waitErr)
	}
}

func TestCancelBeforeStart(t *testing.T) {
	t.Parallel()

	err := cancelGit(&exec.Cmd{})
	if !errors.Is(err, os.ErrProcessDone) {
		t.Fatal("unexpected cancellation result", err)
	}
}
