//nolint:testpackage // Intentionally withhold the reader and drain private batches to overflow the kernel queue.
package fanotify

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
)

// TestKernelQueueOverflow deliberately withholds reads from a private fanotify group.
// No host sysctls are changed. Ordinary go test runs skip this workload.
//
//nolint:gocognit // Keep the ordered kernel overflow workload and recovery assertions in one opt-in test.
func TestKernelQueueOverflow(t *testing.T) {
	t.Parallel()

	if os.Getenv("FSLEDGER_KERNEL_OVERFLOW") != "1" {
		t.Skip("opt in with FSLEDGER_KERNEL_OVERFLOW=1; creates max_queued_events+1 temporary files")
	}

	limit := kernelQueueLimit(t)
	source := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(source, matcher)
	if err != nil {
		t.Skipf("fanotify unavailable: %v", err)
	}
	defer resource.Close(detector)

	markErr := detector.markTree(source, nil)
	if markErr != nil {
		t.Fatal(markErr)
	}

	for index := range limit + 1 {
		writeErr := os.WriteFile(filepath.Join(source, strconv.Itoa(index)), []byte("overflow fixture"), 0o600)
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	}

	buffer := make([]byte, eventBufferSize)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		readErr := detector.readBatch(buffer)
		if readErr != nil {
			t.Fatal(readErr)
		}

		for len(detector.queue.Channel) > 0 {
			raw := <-detector.queue.Channel
			resource.OptionalFile(raw.PIDFD)
		}

		if detector.Dirty() {
			if reason := detector.LossReason(); reason != "FAN_Q_OVERFLOW" {
				t.Fatal("expected kernel overflow, got", reason)
			}

			t.Logf("real FAN_Q_OVERFLOW received after %d unique files", limit+1)

			return
		}
	}

	t.Fatal("real kernel queue overflow was not reported")
}

func kernelQueueLimit(t *testing.T) int {
	t.Helper()

	data, err := os.ReadFile("/proc/sys/fs/fanotify/max_queued_events")
	if err != nil {
		t.Skipf("fanotify queue limit unavailable: %v", err)
	}

	limit, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}

	const maximumTestFiles = 65536
	if limit < 1 || limit >= maximumTestFiles {
		t.Skipf("queue limit %d exceeds the bounded test workload", limit)
	}

	return limit
}
