package git

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestCommandFailureOmitsArgumentsAndPreservesCause(t *testing.T) {
	t.Parallel()
	repository := Repository{Path: t.TempDir(), Host: "privacy-test", publishable: "", Timeout: 0}

	_, err := repository.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	const sensitiveArgument = "fixture-private-revision"

	output, err := repository.run(t.Context(), "rev-parse", "--verify", sensitiveArgument)
	if err == nil || len(output) != 0 || strings.Contains(err.Error(), sensitiveArgument) {
		t.Fatal("failed Git command exposed its argument or output")
	}

	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() == 0 || len(exitError.Stderr) != 0 {
		t.Fatal("Git exit status was lost or stderr escaped through the error chain")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = repository.run(ctx, "rev-parse", "--verify", sensitiveArgument)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), sensitiveArgument) {
		t.Fatal("Git cancellation was lost or exposed its argument")
	}
}
