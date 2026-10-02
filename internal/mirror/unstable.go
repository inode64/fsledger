package mirror

import (
	"context"
	"errors"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
)

func (s *Sync) copyStable(ctx context.Context, path, relative string, mode copyMode) error {
	const attempts = 3

	for range attempts {
		err := ctx.Err()
		if err != nil {
			return fault.Wrap("copy cancelled", err)
		}

		err = s.copy(ctx, path, relative, mode)
		if !errors.Is(err, integrity.ErrUnstable) {
			return err
		}
	}

	return integrity.UnstablePathsError{path}
}
