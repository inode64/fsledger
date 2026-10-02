package integrity

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/inode64/fsledger/internal/fault"
)

func TestSplitUnstablePreservesTerminalWrappedFailures(t *testing.T) {
	t.Parallel()

	failure := fault.New("cannot read source")
	for _, err := range []error{
		failure,
		fmt.Errorf("scan: %w", failure),
		errors.Join(UnstablePathsError{"/a"}, failure),
		fmt.Errorf("scan: %w", errors.Join(UnstablePathsError{"/a"}, failure)),
	} {
		_, fatal := SplitUnstable(err)
		if !errors.Is(fatal, failure) {
			t.Fatalf("operational failure discarded: input %v, got %v", err, fatal)
		}
	}
}

func TestSplitUnstablePreservesOperationalFailures(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("scan: %w", errors.Join(UnstablePathsError{"/b", "/a"}, UnstablePathsError{"/b"}))

	paths, fatal := SplitUnstable(err)
	if fatal != nil || !reflect.DeepEqual(paths, []string{"/a", "/b"}) || !errors.Is(err, ErrUnstable) {
		t.Fatal(paths, fatal, err)
	}

	paths, fatal = SplitUnstable(errors.Join(err, context.Canceled))
	if len(paths) != 2 || !errors.Is(fatal, context.Canceled) {
		t.Fatal("cancellation hidden by unstable path", paths, fatal)
	}

	_, fatal = SplitUnstable(ErrUnstable)
	if fatal == nil {
		t.Fatal("unlocated instability silently discarded")
	}
}

func TestConsumeRetainsEveryFailure(t *testing.T) {
	t.Parallel()

	results := make(chan observation, 3)
	results <- observation{record: Record{}, err: UnstablePathsError{"/a"}}

	results <- observation{record: Record{}, err: UnstablePathsError{"/b"}}

	results <- observation{record: Record{}, err: context.Canceled}

	close(results)

	err := consume(results, func(Record) error { return nil }, func() {})

	paths, fatal := SplitUnstable(err)
	if len(paths) != 2 || !errors.Is(fatal, context.Canceled) {
		t.Fatal(paths, fatal)
	}
}
