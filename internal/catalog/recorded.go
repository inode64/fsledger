package catalog

import (
	"context"
	"path/filepath"

	"github.com/inode64/fsledger/internal/pathutil"
)

// RecordedOutside returns the topmost inventory paths outside roots, in path order. After a restart
// they are the only record of sources that an incomplete selection could not read.
func (store *Store) RecordedOutside(ctx context.Context, roots []string) ([]string, error) {
	var outside []string

	tops := make(map[string]struct{})

	// Keys are ordered, so an ancestor is always visited before its descendants.
	err := store.iterate(ctx, currentPrefix, func(path, _ []byte) error {
		name := string(path)
		if pathutil.Within(roots, name) || belowTop(tops, name) {
			return nil
		}

		tops[name] = struct{}{}
		outside = append(outside, name)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return outside, nil
}

// belowTop walks ancestors instead of comparing every top, which may be one per file without directories.
func belowTop(tops map[string]struct{}, path string) bool {
	for {
		_, found := tops[path]
		if found || path == filepath.Dir(path) {
			return found
		}

		path = filepath.Dir(path)
	}
}
