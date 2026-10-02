package mirror

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
)

func TestComparisonReusesBuffersAcrossSizes(t *testing.T) {
	t.Parallel()

	syncer := &Sync{}

	for _, size := range []int{17, chunkBytes + 1, 1, 0, chunkBytes - 1} {
		content := strings.Repeat("x", size)

		equal, err := syncer.equalContent(
			t.Context(), strings.NewReader(content), strings.NewReader(content), int64(size),
		)
		if err != nil || !equal {
			t.Fatalf("equal content of size %d: %v, %v", size, equal, err)
		}

		if size == 0 {
			continue
		}

		for _, other := range []string{content[:size-1], content[:size-1] + "y"} {
			equal, err = syncer.equalContent(
				t.Context(), strings.NewReader(content), strings.NewReader(other), int64(size),
			)
			if err != nil || equal {
				t.Fatalf("different content of size %d: %v, %v", size, equal, err)
			}
		}
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := syncer.equalContent(cancelled, strings.NewReader("x"), strings.NewReader("x"), 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("comparison ignored cancellation", err)
	}
}

func TestEventVerifiesBytesDespiteMatchingSignature(t *testing.T) {
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

	syncer, err := Open(repository, []string{source}, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(syncer)

	err = syncer.ApplyPaths(t.Context(), []string{source})
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(source, []byte("modified"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a filesystem signature collision, without claiming to reproduce one on this host.
	cached, _ := syncer.cache.Get(source)

	cached.source, err = metadataSignature(info)
	if err != nil {
		t.Fatal(err)
	}

	syncer.cache.Set(source, cached)

	err = syncer.ApplyPaths(t.Context(), []string{source})
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // Read only the mirror entry in this test's private repository.
	data, err := os.ReadFile(filepath.Join(repository, strings.TrimPrefix(source, "/")))
	if err != nil || string(data) != "modified" {
		t.Fatalf("event trusted metadata instead of bytes: %q %v", data, err)
	}
}
