package fanotify_test

import (
	"os"
	"strconv"
	"testing"

	"github.com/inode64/fsledger/internal/watcher/fanotify"
)

// TestKernelPIDFDLifetime validates the production startup probe independently of S1.
// FSLEDGER_KERNEL_PIDFD_REAPED must be true for a corrected kernel, false otherwise.
func TestKernelPIDFDLifetime(t *testing.T) {
	t.Parallel()

	expect := os.Getenv("FSLEDGER_KERNEL_PIDFD_REAPED")
	if expect == "" {
		t.Skip("opt in with FSLEDGER_KERNEL_PIDFD_REAPED=true or false; requires root and fanotify")
	}

	if os.Geteuid() != 0 {
		t.Skip("kernel pidfd lifetime probe requires root")
	}

	want, err := strconv.ParseBool(expect)
	if err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()
	result := fanotify.ProbePIDFDLifetime(t.Context(), parent)
	t.Logf("result=%+v warning=%s", result, result.Warning())

	if !result.Checked || result.Available != want {
		t.Fatal("kernel result did not match explicit expectation")
	}

	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe left temporary files: %v %v", entries, err)
	}
}
