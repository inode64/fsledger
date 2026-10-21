package inbound_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/inode64/fsledger/internal/inbound"
)

func interruptedTemporary(state fixture, plan *inbound.Plan) string {
	return filepath.Join(state.source, ".fsledger-inbound-"+plan.ID+"-"+strconv.Itoa(plan.Done))
}

func TestResumeConflictRemovesInterruptedTemporary(t *testing.T) {
	t.Parallel()
	state := newFixture(t)
	write(t, state.archived("a"), "remote a")
	write(t, state.archived("b"), "remote b")

	plan, err := state.prepare(t)
	if err != nil {
		t.Fatal(err)
	}

	// The first replacement was saved, then a crash interrupted the second one.
	write(t, filepath.Join(state.source, "a"), "remote a")

	plan.Done = 1
	temporary := interruptedTemporary(state, plan)
	write(t, temporary, "remote b")
	write(t, filepath.Join(state.source, "a"), "edited before recovery")

	err = plan.Apply(t.Context(), state.repository, func() error { return nil })
	if !errors.Is(err, inbound.ErrConflict) {
		t.Fatal("recovery overwrote a completed path", err)
	}

	_, err = os.Lstat(temporary)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conflicting recovery left remote content in the source tree", err)
	}

	assertContent(t, filepath.Join(state.source, "b"), "initial")
}

func TestClearTemporaryNeverCreatesParents(t *testing.T) {
	t.Parallel()
	state := newFixture(t)
	write(t, state.archived("nested/new"), "remote")

	plan, err := state.prepare(t)
	if err != nil {
		t.Fatal(err)
	}

	err = plan.ClearTemporary()
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Lstat(filepath.Join(state.source, "nested"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoning an unstarted plan created its parent", err)
	}
}
