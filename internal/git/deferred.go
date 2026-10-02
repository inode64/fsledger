package git

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/pathutil"
)

// Stage updates the index; nil paths selects a complete reconciliation.
func (r *Repository) Stage(ctx context.Context, paths []string) error {
	if paths != nil {
		return r.stage(ctx, paths)
	}

	_, err := r.run(ctx, "add", "--all", "--force", "--", ".")

	return err
}

// PruneUnreachable deletes loose objects that no ref, reflog or index entry reaches.
// Restaging a deferred path supersedes its previous blob without committing, so Git's
// automatic gc never runs and its default expiry would keep those blobs for weeks.
// The caller must serialize repository access: an immediate expiry is unsafe otherwise.
func (r *Repository) PruneUnreachable(ctx context.Context) error {
	_, err := r.run(ctx, "prune", "--expire=now")

	return err
}

// StagedVersions returns exact index changes, including modes and deletions.
// NUL framing preserves arbitrary filenames; rename detection is deliberately disabled.
func (r *Repository) StagedVersions(ctx context.Context) (map[string]string, error) {
	out, err := r.run(ctx, "diff", "--cached", "--raw", "-z", "--no-renames", "--abbrev=64")
	if err != nil {
		return nil, err
	}

	return parseVersions(string(out))
}

const stagedFieldCount = 5

func parseVersions(output string) (map[string]string, error) {
	result := make(map[string]string)

	for output != "" {
		header, rest, found := strings.Cut(output, "\x00")
		if !found {
			return nil, fault.New("invalid staged diff header")
		}

		path, remaining, found := strings.Cut(rest, "\x00")
		if !found || !strings.HasPrefix(header, ":") || len(strings.Fields(header)) != stagedFieldCount ||
			!pathutil.ValidRelative(path) {
			return nil, fault.New("invalid staged diff path")
		}

		result["/"+path] = header
		output = remaining
	}

	return result, nil
}

// Quoting keeps line breaks and arbitrary filename bytes unambiguous in messages.
func describeVersions(versions map[string]string) (string, error) {
	lines := make([]string, 0, len(versions))
	for _, path := range slices.Sorted(maps.Keys(versions)) {
		fields := strings.Fields(versions[path])
		if len(fields) != stagedFieldCount {
			return "", fault.New("invalid staged diff header")
		}

		lines = append(lines, fields[stagedFieldCount-1]+"\t"+strconv.Quote(strings.TrimPrefix(path, "/"))+"\n")
	}

	return strings.Join(lines, ""), nil
}
