package git_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHasHistoryDistinguishesUnbornHeadFromFailure(t *testing.T) {
	t.Parallel()

	repository := newRepository(t)

	history, err := repository.HasHistory(t.Context())
	if err != nil || history {
		t.Fatalf("unborn HEAD: history=%v error=%v", history, err)
	}

	writeRepositoryFile(t, repository, "etc/motd", "v1")
	commitDated(t, repository, time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC), "first")

	history, err = repository.HasHistory(t.Context())
	if err != nil || !history {
		t.Fatalf("committed HEAD: history=%v error=%v", history, err)
	}

	err = os.WriteFile(filepath.Join(repository.Path, ".git", "HEAD"), []byte("not-a-reference\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	history, err = repository.HasHistory(t.Context())
	if err == nil || history {
		t.Fatalf("corrupt HEAD treated as a verdict: history=%v error=%v", history, err)
	}
}
