package mirror

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/inode64/fsledger/internal/filehandle"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

// walkDirectories keeps, during one reconciliation walk, the source and mirror directories whose entries
// are being copied: resolving both from their roots for every file dominated cached reconciliations. The
// source directory is the anchored descriptor the first entry copied since the walk last entered it opened;
// a subdirectory replaces it, and the next sibling anchors its parent again. No symlink is ever followed,
// and the descriptor serves only while the kernel still names its directory with the path: one moved away,
// such as into an excluded directory or out of the source, is anchored again from the root.
type walkDirectories struct {
	mirrorRoot *os.Root
	source     string
	mirror     string
	sourceFD   int
}

func newWalkDirectories() *walkDirectories {
	return &walkDirectories{mirrorRoot: nil, source: "", mirror: "", sourceFD: -1}
}

func (cache *walkDirectories) close() {
	cache.forgetMirror()

	if cache.sourceFD >= 0 {
		resource.FD(cache.sourceFD)
		cache.source, cache.sourceFD = "", -1
	}
}

// sourceStill reports whether the kept source directory is still the one its path names: procfs names a
// descriptor's directory by its current location, which has no symlinks.
func (cache *walkDirectories) sourceStill() bool {
	current, err := filehandle.DescriptorPath(cache.sourceFD)

	return err == nil && current == cache.source
}

func (cache *walkDirectories) forgetMirror() {
	if cache.mirrorRoot != nil {
		resource.Close(cache.mirrorRoot)
		cache.mirror, cache.mirrorRoot = "", nil
	}
}

// sourceParent anchors path's parent without following symlinks; release closes it unless the walk keeps it.
func (s *Sync) sourceParent(path string) (int, string, func(), error) {
	cache := s.walkDirectories
	directory := filepath.Dir(path)

	if cache != nil && cache.sourceFD >= 0 && cache.source == directory && cache.sourceStill() {
		return cache.sourceFD, filepath.Base(path), func() {}, nil
	}

	parent, name, err := integrity.OpenParent(path)
	if err != nil {
		return -1, "", func() {}, err
	}

	if cache == nil {
		return parent, name, func() { resource.FD(parent) }, nil
	}

	if cache.sourceFD >= 0 {
		resource.FD(cache.sourceFD)
	}

	cache.source, cache.sourceFD = directory, parent

	return parent, name, func() {}, nil
}

// mirrorLstat describes a mirror entry without following it, through the walk's mirror directory.
func (s *Sync) mirrorLstat(relative string) (fs.FileInfo, error) {
	cache := s.walkDirectories
	directory := filepath.Dir(relative)

	if cache == nil || directory == "." {
		return s.root.Lstat(relative) //nolint:wrapcheck // Callers wrap with their operation.
	}

	if cache.mirrorRoot == nil || cache.mirror != directory {
		cache.forgetMirror()

		root, err := s.root.OpenRoot(directory)
		if err != nil {
			return s.root.Lstat(relative) //nolint:wrapcheck // As above.
		}

		cache.mirror, cache.mirrorRoot = directory, root
	}

	return cache.mirrorRoot.Lstat(filepath.Base(relative)) //nolint:wrapcheck // As above.
}
