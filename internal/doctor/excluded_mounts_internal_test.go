package doctor

import (
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/mountinfo"
)

// The daemon never watches an excluded mount point, so the diagnosis must neither list nor probe it.
func TestInspectRootSkipsExcludedMounts(t *testing.T) {
	t.Parallel()

	matcher, err := exclude.New("/srv/site/cache")
	if err != nil {
		t.Fatal(err)
	}

	mounts := []mountinfo.Mount{
		{Point: "/", Type: "ext4"},
		{Point: "/srv/site/cache", Type: "tmpfs"},
		{Point: "/srv/site/cache/deep", Type: "tmpfs"},
		{Point: "/srv/site/data", Type: "xfs"},
	}
	cfg := &config.Config{}
	cfg.Watch.Backend = event.Polling

	inspected := inspectRoot(cfg, matcher, "site", "/srv/site", mounts)

	paths := make([]string, 0, len(inspected))
	for _, path := range inspected {
		paths = append(paths, path.Path)
	}

	if !slices.Equal(paths, []string{"/srv/site", "/srv/site/data"}) {
		t.Fatalf("excluded mounts were inspected: %v", paths)
	}
}
