package inbound_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/fault"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/inbound"
)

type fixture struct {
	repository *gitrepo.Repository
	matcher    *exclude.Matcher
	source     string
	base       string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	state := fixture{
		source: t.TempDir(), base: "", matcher: nil,
		repository: &gitrepo.Repository{Path: t.TempDir(), Host: "test", Timeout: 0},
	}

	_, err := state.repository.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"a", "b"} {
		write(t, filepath.Join(state.source, name), "initial")
		write(t, state.archived(name), "initial")
	}

	state.base, err = state.repository.Commit(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	state.matcher, err = exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	return state
}

func (state fixture) archived(name string) string {
	return filepath.Join(state.repository.Path, strings.TrimPrefix(state.source, "/"), name)
}

func (state fixture) prepare(t *testing.T) (*inbound.Plan, error) {
	t.Helper()

	target, err := state.repository.Commit(t.Context(), "Incoming change")
	if err != nil {
		t.Fatal(err)
	}

	differences, err := state.repository.TreeChanges(t.Context(), state.base, target)
	if err != nil {
		t.Fatal(err)
	}

	return inbound.Prepare(t.Context(), state.repository, []string{state.source}, state.matcher,
		state.base, target, "policy", differences)
}

func write(t *testing.T, path, value string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(value), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, path, value string) {
	t.Helper()
	//nolint:gosec // The fixture owns this temporary path.
	data, err := os.ReadFile(path)
	if err != nil || string(data) != value {
		t.Fatalf("%q: got %q, want %q, error %v", path, data, value, err)
	}
}

func TestApplyCreatesDeletesAndPreservesRawNames(t *testing.T) {
	t.Parallel()
	state := newFixture(t)
	write(t, state.archived("a"), "remote")

	rawName := "nested/line\nbyte\xff"
	write(t, state.archived(rawName), "raw bytes")

	err := os.Remove(state.archived("b"))
	if err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "outside")
	write(t, outside, "untouched")

	err = os.Symlink(outside, state.archived("link"))
	if err != nil {
		t.Fatal(err)
	}

	plan, err := state.prepare(t)
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}

	var restored inbound.Plan

	err = json.Unmarshal(encoded, &restored)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		err = restored.Apply(t.Context(), state.repository, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}

	assertContent(t, filepath.Join(state.source, "a"), "remote")
	assertContent(t, filepath.Join(state.source, rawName), "raw bytes")
	assertContent(t, outside, "untouched")

	_, err = os.Lstat(filepath.Join(state.source, "b"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deletion not applied", err)
	}

	link, err := os.Readlink(filepath.Join(state.source, "link"))
	if err != nil || link != outside {
		t.Fatal("symlink was not preserved literally", link, err)
	}
}

func TestApplyRejectsConcurrentLocalChange(t *testing.T) {
	t.Parallel()
	state := newFixture(t)
	write(t, state.archived("a"), "remote a")
	write(t, state.archived("b"), "remote b")

	plan, err := state.prepare(t)
	if err != nil {
		t.Fatal(err)
	}

	err = plan.Apply(t.Context(), state.repository, func() error {
		write(t, filepath.Join(state.source, "b"), "concurrent local")

		return nil
	})
	if !errors.Is(err, inbound.ErrConflict) || plan.Done != 1 {
		t.Fatal("local modification not detected", plan.Done, err)
	}

	assertContent(t, filepath.Join(state.source, "b"), "concurrent local")
	write(t, filepath.Join(state.source, "a"), "edited after partial import")

	err = plan.Apply(t.Context(), state.repository, func() error { return nil })
	if !errors.Is(err, inbound.ErrConflict) {
		t.Fatal("recovery overwrote a completed path", err)
	}

	assertContent(t, filepath.Join(state.source, "a"), "edited after partial import")
}

func TestApplyRejectsReplacedRoot(t *testing.T) {
	t.Parallel()
	state := newFixture(t)
	write(t, state.archived("a"), "remote")

	plan, err := state.prepare(t)
	if err != nil {
		t.Fatal(err)
	}

	moved := filepath.Join(t.TempDir(), "old")

	err = os.Rename(state.source, moved)
	if err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(state.source, "a"), "initial")

	err = plan.Apply(t.Context(), state.repository, func() error { return nil })
	if !errors.Is(err, inbound.ErrConflict) {
		t.Fatal("replacement root accepted", err)
	}

	assertContent(t, filepath.Join(state.source, "a"), "initial")
}

func TestIncomingParentSymlinkIsNeverFollowed(t *testing.T) {
	t.Parallel()

	for _, beforePrepare := range []bool{true, false} {
		state := newFixture(t)
		outside := t.TempDir()
		write(t, state.archived("nested/file"), "remote")

		makeLink := func() {
			err := os.Symlink(outside, filepath.Join(state.source, "nested"))
			if err != nil {
				t.Fatal(err)
			}
		}
		if beforePrepare {
			makeLink()
		}

		plan, err := state.prepare(t)
		if !beforePrepare && err == nil {
			makeLink()

			err = plan.Apply(t.Context(), state.repository, func() error { return nil })
		}

		if !errors.Is(err, inbound.ErrConflict) {
			t.Fatal("symlink parent accepted", beforePrepare, err)
		}

		_, err = os.Lstat(filepath.Join(outside, "file"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal("write escaped through symlink", err)
		}
	}
}

func TestIncomingExclusionsRejectWholePlan(t *testing.T) {
	t.Parallel()
	state := newFixture(t)
	write(t, state.archived("a"), "remote")
	write(t, state.archived("blocked"), "forbidden")
	state.matcher = state.matcher.Protect(filepath.Join(state.source, "blocked"))

	_, err := state.prepare(t)
	if !errors.Is(err, inbound.ErrConflict) {
		t.Fatal("excluded change accepted", err)
	}

	assertContent(t, filepath.Join(state.source, "a"), "initial")
}

func TestIncomingPreservesPermissionsAndAttributes(t *testing.T) {
	t.Parallel()
	state := newFixture(t)
	source := filepath.Join(state.source, "a")
	//nolint:gosec // Verify preservation of an existing non-private mode in the temporary source.
	err := os.Chmod(source, 0o640)
	if err != nil {
		t.Fatal(err)
	}

	err = unix.Setxattr(source, "user.fsledger-test", []byte("metadata"), 0)
	if errors.Is(err, unix.ENOTSUP) {
		t.Skip("temporary filesystem does not support user xattrs")
	}

	if err != nil {
		t.Fatal(err)
	}

	write(t, state.archived("a"), "remote")

	plan, err := state.prepare(t)
	if err != nil {
		t.Fatal(err)
	}

	err = plan.Apply(t.Context(), state.repository, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(source)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatal("mode not preserved", info, err)
	}

	value := make([]byte, 32)

	count, err := unix.Getxattr(source, "user.fsledger-test", value)
	if err != nil || string(value[:count]) != "metadata" {
		t.Fatal("xattr not preserved", value, err)
	}
}

func TestIncomingRejectsHardlinksAndLocalEdits(t *testing.T) {
	t.Parallel()

	for _, hardlink := range []bool{true, false} {
		state := newFixture(t)
		write(t, state.archived("a"), "remote")

		if hardlink {
			err := os.Link(filepath.Join(state.source, "a"), filepath.Join(state.source, "alias"))
			if err != nil {
				t.Fatal(err)
			}
		} else {
			write(t, filepath.Join(state.source, "a"), "local")
		}

		_, err := state.prepare(t)
		if !errors.Is(err, inbound.ErrConflict) {
			t.Fatal("unsafe source accepted", hardlink, err)
		}
	}
}

func TestIncomingCompletedProgressIsNotSavedAgain(t *testing.T) {
	t.Parallel()
	state := newFixture(t)
	write(t, state.archived("a"), "remote a")
	write(t, state.archived("b"), "remote b")

	plan, err := state.prepare(t)
	if err != nil {
		t.Fatal(err)
	}

	err = plan.Apply(t.Context(), state.repository, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	completed := plan.Done
	interrupted := fault.New("interrupted recovery")

	err = plan.Apply(t.Context(), state.repository, func() error { return interrupted })
	if err != nil || plan.Done != completed {
		t.Fatal("recovery lost recorded progress", plan.Done, completed, err)
	}
}

type countedObjects struct {
	objects inbound.Objects
	reads   map[string]int
}

func (objects countedObjects) ReadBlob(ctx context.Context, identifier string) ([]byte, error) {
	objects.reads[identifier]++

	return objects.objects.ReadBlob(ctx, identifier)
}

func TestIncomingRetriesFailedProgressSave(t *testing.T) {
	t.Parallel()
	state := newFixture(t)
	write(t, state.archived("a"), "remote a")

	plan, err := state.prepare(t)
	if err != nil {
		t.Fatal(err)
	}

	objects := countedObjects{objects: state.repository, reads: make(map[string]int)}
	failure := fault.New("progress save failed")

	err = plan.Apply(t.Context(), objects, func() error { return failure })
	if !errors.Is(err, failure) || plan.Done != 0 {
		t.Fatal("unsaved progress retained", plan.Done, err)
	}

	assertContent(t, filepath.Join(state.source, "a"), "remote a")

	if objects.reads[plan.Changes[0].After.Object] != 1 {
		t.Fatal("replacement blob read more than once", objects.reads)
	}

	saves := 0

	err = plan.Apply(t.Context(), state.repository, func() error {
		saves++

		return nil
	})
	if err != nil || saves != 1 || plan.Done != len(plan.Changes) {
		t.Fatal("progress not persisted on retry", plan.Done, saves, err)
	}
}
