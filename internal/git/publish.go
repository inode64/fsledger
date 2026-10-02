package git

import (
	"context"

	"github.com/inode64/fsledger/internal/fault"
)

// Publish sends a pinned commit to exactly one branch, without rewriting history.
func (r *Repository) Publish(ctx context.Context, remote, branch, commit string) error {
	// Git is the authority on branch names, but the configured branch needs asking only once.
	if branch != r.publishable {
		_, err := r.run(ctx, "check-ref-format", "--branch", branch)
		if err != nil {
			return fault.New("invalid publication branch")
		}

		r.publishable = branch
	}
	// Restrict the refspec source to a full object ID, never caller-supplied refspec syntax.
	if !objectID(commit) {
		return fault.New("publication requires a full commit object ID")
	}

	_, err := r.run(ctx, "-c", "push.followTags=false", "-c", "remote."+remote+".mirror=false",
		"push", "--porcelain", "--no-follow-tags", "--recurse-submodules=no", "--", remote,
		commit+":refs/heads/"+branch)
	if err != nil {
		// Git stderr can contain credentials emitted by administrator hooks/helpers.
		return fault.New("Git publication failed; check connectivity, credentials and branch protection")
	}

	return nil
}
