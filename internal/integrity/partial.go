package integrity

import (
	"errors"
	"slices"
	"strings"
)

// UnstablePathsError identifies observations deferred after bounded stability retries.
// Callers that require every path to be observed still treat it as an error.
type UnstablePathsError []string

func (paths UnstablePathsError) Error() string {
	return ErrUnstable.Error() + ": " + strings.Join(paths, ", ")
}

func (UnstablePathsError) Unwrap() error { return ErrUnstable }

// SplitUnstable separates explicitly deferred paths from operational failures.
// In particular, a joined cancellation, I/O or root error must never become a success.
func SplitUnstable(err error) ([]string, error) {
	var paths []string

	failure := splitUnstable(err, &paths)
	slices.Sort(paths)

	return slices.Compact(paths), failure
}

//nolint:errorlint // Inspect every unwrap branch; errors.As would hide a fatal sibling of a partial result.
func splitUnstable(err error, paths *[]string) error {
	switch value := err.(type) {
	case nil:
		return nil
	case UnstablePathsError:
		*paths = append(*paths, value...)

		return nil
	case interface{ Unwrap() []error }:
		var failures []error

		for _, cause := range value.Unwrap() {
			failures = append(failures, splitUnstable(cause, paths))
		}

		return errors.Join(failures...)
	case interface{ Unwrap() error }:
		cause := value.Unwrap()
		if cause != nil && splitUnstable(cause, paths) == nil {
			return nil
		}
	}

	return err
}
