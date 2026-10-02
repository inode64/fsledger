package git_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitrepo "github.com/inode64/fsledger/internal/git"
)

//nolint:cyclop,funlen,gocognit,gocyclo // The full lifecycle keeps ordered filesystem/Git assertions together.
func TestCommits(t *testing.T) {
	t.Parallel()

	_, lookPathErr := exec.LookPath("git")
	if lookPathErr != nil {
		t.Skip("git not installed")
	}

	repository := &gitrepo.Repository{Path: t.TempDir(), Host: testHost, Timeout: 0}

	initial, err := repository.Init(t.Context())
	if err != nil || !initial {
		t.Fatalf("init: %v %v", initial, err)
	}

	path := filepath.Join(repository.Path, "file")

	writeFileErr := os.WriteFile(path, []byte("one"), 0o600)
	if writeFileErr != nil {
		t.Fatal(writeFileErr)
	}

	hash, err := repository.Commit(t.Context(), "Initial snapshot for testhost")
	if err != nil || hash == "" {
		t.Fatalf("initial commit: %s %v", hash, err)
	}

	hash, err = repository.Commit(t.Context(), "empty")
	if err != nil || hash != "" {
		t.Fatalf("empty commit: %s %v", hash, err)
	}

	writeFileErr2 := os.WriteFile(path, []byte("actor A"), 0o600)
	if writeFileErr2 != nil {
		t.Fatal(writeFileErr2)
	}

	_, commitErr := repository.Commit(t.Context(), "actor A")
	if commitErr != nil {
		t.Fatal(commitErr)
	}

	writeFileErr3 := os.WriteFile(filepath.Join(repository.Path, "second"), []byte("actor B"), 0o600)
	if writeFileErr3 != nil {
		t.Fatal(writeFileErr3)
	}

	_, commitErr2 := repository.Commit(t.Context(), "actor B")
	if commitErr2 != nil {
		t.Fatal(commitErr2)
	}

	removeErr := os.Remove(path)
	if removeErr != nil {
		t.Fatal(removeErr)
	}

	_, commitErr3 := repository.Commit(t.Context(), "delete")
	if commitErr3 != nil {
		t.Fatal(commitErr3)
	}

	//nolint:gosec // This test uses only its private temporary directory and the current test process.
	output, err := exec.CommandContext(t.Context(), "git", "-C", repository.Path, "log", "--format=%s").Output()
	if err != nil {
		t.Fatal(err)
	}

	if string(output) != "delete\nactor B\nactor A\nInitial snapshot for testhost\n" {
		t.Fatalf("unexpected commits: %s", output)
	}

	status, err := repository.Status(t.Context())
	if err != nil || strings.TrimSpace(status) != "" {
		t.Fatalf("dirty worktree: %s %v", status, err)
	}
}

func TestStatusDoesNotRefreshIndexOnDisk(t *testing.T) {
	t.Parallel()
	repository := &gitrepo.Repository{Path: t.TempDir(), Host: testHost, Timeout: 0}

	_, err := repository.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(repository.Path, "file")

	err = os.WriteFile(path, []byte("unchanged"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.Commit(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	index := filepath.Join(repository.Path, ".git", "index")

	//nolint:gosec // The index belongs to this test's private temporary repository.
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	// Change stat data without changing content, forcing an optional refresh.
	changedTime := time.Now().Add(time.Hour)

	err = os.Chtimes(path, changedTime, changedTime)
	if err != nil {
		t.Fatal(err)
	}

	status, err := repository.Status(t.Context())
	if err != nil || status != "" {
		t.Fatal("unexpected status", status, err)
	}

	//nolint:gosec // The index belongs to this test's private temporary repository.
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(before, after) {
		t.Fatal("status wrote the index")
	}
}

func TestInitRejectsCorruptHEAD(t *testing.T) {
	t.Parallel()
	repository := &gitrepo.Repository{Path: t.TempDir(), Host: testHost, Timeout: 0}

	_, err := repository.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(repository.Path, ".git", "HEAD"), []byte("not-a-reference\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	initial, err := repository.Init(t.Context())
	if err == nil || initial {
		t.Fatalf("corrupt HEAD treated as unborn: initial=%v error=%v", initial, err)
	}
}
