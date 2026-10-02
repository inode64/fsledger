package mirror

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
)

//nolint:funlen,gocognit // Ordered fixture: the second rewrite depends on the state left by the first.
func TestReconcileVerifiesRacyTimestamps(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "file")

	err := os.WriteFile(source, []byte("original"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	repository := t.TempDir()

	syncer, err := Open(repository, []string{source}, matcher, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(syncer)

	mirrored := filepath.Join(repository, strings.TrimPrefix(source, "/"))

	// Simulate a same-tick rewrite whose event was lost: the signature still matches.
	rewrite := func(content string, verified int64) string {
		t.Helper()

		writeErr := os.WriteFile(source, []byte(content), 0o600)
		if writeErr != nil {
			t.Fatal(writeErr)
		}

		info, statErr := os.Stat(source)
		if statErr != nil {
			t.Fatal(statErr)
		}

		cached, _ := syncer.cache.Get(source)
		cached.verified = verified

		cached.source, statErr = metadataSignature(info)
		if statErr != nil {
			t.Fatal(statErr)
		}

		syncer.cache.Set(source, cached)

		reconcileErr := syncer.ReconcileContext(t.Context())
		if reconcileErr != nil {
			t.Fatal(reconcileErr)
		}

		//nolint:gosec // Read only the mirror entry in this test's private repository.
		got, readErr := os.ReadFile(mirrored)
		if readErr != nil {
			t.Fatal(readErr)
		}

		return string(got)
	}

	err = syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	const rewritten = "rewrite1"

	if got := rewrite(rewritten, time.Now().UnixNano()); got != rewritten {
		t.Fatalf("racy signature skipped the copy: %q", got)
	}

	// Content verified long after the timestamps is trusted without reading bytes.
	if got := rewrite("rewrite2", time.Now().Add(time.Hour).UnixNano()); got != rewritten {
		t.Fatalf("settled signature reread the source: %q", got)
	}
}
