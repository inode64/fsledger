package git

import (
	"context"
	"strconv"
	"strings"

	"github.com/inode64/fsledger/internal/fault"
)

// boundedBlob reads literal immutable bytes. Oversized blobs are distinguished
// from failures so each caller can skip them or reject its operation.
func (r *Repository) boundedBlob(ctx context.Context, identifier string, limit int64) ([]byte, bool, error) {
	out, err := r.run(ctx, "cat-file", "-s", identifier)
	if err != nil {
		return nil, false, err
	}

	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || size < 0 {
		return nil, false, fault.New("invalid Git blob size")
	}

	if size > limit {
		return nil, false, nil
	}

	content, err := r.run(ctx, "cat-file", "blob", identifier)

	return content, err == nil, err
}
