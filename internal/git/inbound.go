package git

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/inode64/fsledger/internal/fault"
)

const incomingRef = "refs/fsledger/incoming"

// ErrIncomingTreeChanged distinguishes local content conflicts from retryable Git failures.
var ErrIncomingTreeChanged = errors.New("source tree changed during incoming synchronization")

// MaxInboundBlob bounds memory used for a single source replacement.
const MaxInboundBlob = 64 << 20

// FetchBranch fetches only the configured branch, never the worktree or tags.
// An empty result means the remote branch does not exist yet.
func (r *Repository) FetchBranch(ctx context.Context, remote, branch string) (string, error) {
	_, err := r.run(ctx, "check-ref-format", "refs/heads/"+branch)
	if err != nil {
		return "", fault.New("invalid synchronization branch")
	}

	out, err := r.run(ctx, "ls-remote", "--refs", "--", remote, "refs/heads/"+branch)
	if err != nil {
		return "", fault.New("Git fetch failed; check connectivity, credentials and branch access")
	}

	if len(out) == 0 {
		return "", nil
	}

	_, err = r.run(ctx, "-c", "core.hooksPath=/dev/null", "-c", "fetch.fsckObjects=true",
		"fetch", "--no-tags", "--no-prune", "--no-prune-tags", "--no-recurse-submodules", "--no-auto-gc",
		"--refmap=", "--", remote, "refs/heads/"+branch+":"+incomingRef)
	if err != nil {
		return "", fault.New("Git fetch failed; check connectivity, credentials and branch access")
	}

	out, err = r.run(ctx, "rev-parse", "--verify", incomingRef+"^{commit}")

	return strings.TrimSpace(string(out)), err
}

// IsAncestor distinguishes divergence from an execution failure.
func (r *Repository) IsAncestor(ctx context.Context, base, target string) (bool, error) {
	if !objectID(base) || !objectID(target) {
		return false, fault.New("ancestry requires full object IDs")
	}

	_, err := r.run(ctx, "merge-base", "--is-ancestor", base, target)
	if missingGitObject(err) {
		return false, nil
	}

	return err == nil, err
}

// TreeChanges reads immutable raw tree differences with byte-preserving paths.
func (r *Repository) TreeChanges(ctx context.Context, base, target string) (map[string]string, error) {
	if !objectID(base) || !objectID(target) {
		return nil, fault.New("tree comparison requires full object IDs")
	}

	out, err := r.run(ctx, "diff-tree", "--no-ext-diff", "--no-textconv", "--no-renames",
		"-r", "--raw", "-z", "--no-commit-id", "--abbrev=64", base, target, "--")
	if err != nil {
		return nil, err
	}

	return parseVersions(string(out))
}

// ReadBlob returns literal immutable bytes, without filters or text conversion.
func (r *Repository) ReadBlob(ctx context.Context, identifier string) ([]byte, error) {
	if !objectID(identifier) {
		return nil, fault.New("blob read requires a full object ID")
	}

	content, eligible, err := r.boundedBlob(ctx, identifier, MaxInboundBlob)
	if err != nil {
		return nil, err
	}

	if !eligible {
		return nil, fault.New("incoming blob exceeds 64 MiB")
	}

	return content, nil
}

// AdoptIncoming moves HEAD only when the staged mirror equals the target tree.
// No checkout or reset ever writes to source paths.
func (r *Repository) AdoptIncoming(ctx context.Context, base, target string) error {
	if !objectID(base) || !objectID(target) {
		return fault.New("incoming adoption requires full object IDs")
	}

	err := r.Stage(ctx, nil)
	if err != nil {
		return err
	}

	actual, err := r.run(ctx, "write-tree")
	if err != nil {
		return err
	}

	expected, err := r.run(ctx, "rev-parse", target+"^{tree}")
	if err != nil {
		return err
	}

	if !bytes.Equal(actual, expected) {
		return ErrIncomingTreeChanged
	}

	_, err = r.run(ctx, "-c", "core.hooksPath=/dev/null", "update-ref", "-m", "fsledger incoming", "HEAD", target, base)

	return err
}

// PinIncoming keeps a resumable operation reachable independently of later fetches.
func (r *Repository) PinIncoming(ctx context.Context, target string) error {
	if !objectID(target) {
		return fault.New("incoming pin requires a full object ID")
	}

	_, err := r.run(ctx, "-c", "core.hooksPath=/dev/null", "update-ref", "refs/fsledger/pending", target)

	return err
}

func objectID(value string) bool {
	const sha1Length, sha256Length = 40, 64

	return (len(value) == sha1Length || len(value) == sha256Length) &&
		!strings.ContainsFunc(value, func(char rune) bool { return !strings.ContainsRune("0123456789abcdef", char) })
}
