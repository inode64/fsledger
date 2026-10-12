// Package restore writes a Git archive into a destination directory under an overwrite policy.
package restore

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// ErrSourceMissing reports that the archive never contained the requested prefix.
var ErrSourceMissing = errors.New("archived path not found")

// Policy decides what happens when a destination entry already exists.
type Policy int

// Overwrite policies accepted by --overwrite.
const (
	Off Policy = iota
	Always
	Newer
)

// ParsePolicy maps the command line spelling to a Policy.
func ParsePolicy(value string) (Policy, error) {
	switch value {
	case "off":
		return Off, nil
	case "always":
		return Always, nil
	case "newer":
		return Newer, nil
	default:
		return Off, fault.New("--overwrite must be off, always or newer")
	}
}

// Options describe one restore run.
type Options struct {
	// Committed dates the archived commit; Newer compares it with destination modification times.
	Committed time.Time
	// Output receives one line per archived entry under Prefix.
	Output io.Writer
	// Destination is where Prefix lands: a directory when Prefix is a tree, otherwise the new path
	// of the single archived file or symlink.
	Destination string
	// Prefix is the archived path being restored; ancestors and siblings are ignored.
	Prefix string
	Policy Policy
	DryRun bool
}

// Summary counts the outcome of every archived entry under the prefix.
type Summary struct {
	Restored int
	Skipped  int
	Failed   int
}

const (
	fileMode        = 0o644
	executableMode  = 0o755
	executableBits  = 0o111
	directoryMode   = 0o755
	temporaryPrefix = ".fsledger-restore-"
)

type action int

const (
	actionNone action = iota
	actionRestore
	actionSkip
	actionFail
)

// run holds the destination state discovered from the archive's prefix entry.
type run struct {
	// root is nil before the prefix entry arrives, and stays nil in a dry run over an absent destination.
	root *os.Root
	// base is the opened directory shown in output lines.
	base string
	// name is the destination base name when Prefix is a single file or symlink.
	name    string
	options Options
	opened  bool
}

// Apply walks the archive and restores every entry under Options.Prefix. The prefix entry itself
// decides the destination layout, so nothing is created before it is seen.
func Apply(ctx context.Context, reader *tar.Reader, options Options) (Summary, error) {
	state := run{root: nil, base: "", name: "", options: options, opened: false}
	defer state.close()

	var summary Summary

	for {
		err := ctx.Err()
		if err != nil {
			return summary, fault.Wrap("restore cancelled", err)
		}

		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return summary, state.finish()
		}

		if err != nil {
			return summary, fault.Wrap("read archive", err)
		}

		err = state.step(reader, header, &summary)
		if err != nil {
			return summary, err
		}
	}
}

// finish reports an archive that ended without the prefix entry.
func (s *run) finish() error {
	if !s.opened {
		return ErrSourceMissing
	}

	return nil
}

// step handles one archive entry and records its outcome.
func (s *run) step(reader io.Reader, header *tar.Header, summary *Summary) error {
	relative, included, err := s.locate(header)
	if err != nil || !included {
		return err
	}

	outcome, reason := s.entry(reader, header, relative)

	return s.report(summary, outcome, relative, reason)
}

func (s *run) close() {
	if s.root != nil {
		resource.Close(s.root)
	}
}

// locate maps an archive name to a destination path. The prefix entry opens the destination;
// ancestors, siblings and the prefix directory itself are excluded.
func (s *run) locate(header *tar.Header) (string, bool, error) {
	clean := path.Clean(strings.TrimSuffix(header.Name, "/"))

	if s.opened {
		if s.name != "" {
			return "", false, nil
		}

		relative, included := strings.CutPrefix(clean, s.options.Prefix+"/")

		return relative, included, nil
	}

	if clean != s.options.Prefix {
		return "", false, nil
	}

	s.opened = true

	if header.Typeflag == tar.TypeDir {
		return "", false, s.open(s.options.Destination, true)
	}

	s.name = filepath.Base(s.options.Destination)

	return s.name, true, s.open(filepath.Dir(s.options.Destination), false)
}

// open prepares the destination directory. A dry run neither creates it nor fails when it is absent.
func (s *run) open(directory string, create bool) error {
	s.base = directory

	if create && !s.options.DryRun {
		err := os.MkdirAll(directory, directoryMode)
		if err != nil {
			return fault.Wrap("create destination", err)
		}
	}

	root, err := os.OpenRoot(directory)
	if err != nil {
		if s.options.DryRun && errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fault.Wrap("open destination", err)
	}

	s.root = root

	return nil
}

func (s *run) entry(reader io.Reader, header *tar.Header, relative string) (action, string) {
	if !filepath.IsLocal(relative) {
		return actionFail, "invalid path"
	}

	kind, supported := entryKind(header.Typeflag)
	if !supported {
		return actionFail, "unsupported entry type"
	}

	existing, outcome, reason := s.decide(relative, kind)
	if outcome != actionRestore {
		return outcome, reason
	}

	if kind == fs.ModeDir && existing != nil {
		return actionNone, "" // Existing directories are reused silently.
	}

	if s.options.DryRun {
		return actionRestore, ""
	}

	err := s.write(reader, header, relative, kind, existing)
	if err != nil {
		return actionFail, err.Error()
	}

	return actionRestore, ""
}

func entryKind(flag byte) (fs.FileMode, bool) {
	switch flag {
	case tar.TypeReg:
		return 0, true
	case tar.TypeDir:
		return fs.ModeDir, true
	case tar.TypeSymlink:
		return fs.ModeSymlink, true
	default:
		return 0, false
	}
}

// decide applies the policy to the destination entry; existing is non-nil only when it is replaced.
func (s *run) decide(relative string, kind fs.FileMode) (fs.FileInfo, action, string) {
	if s.root == nil {
		return nil, actionRestore, ""
	}

	existing, err := s.root.Lstat(relative)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, actionRestore, ""
	}

	if err != nil {
		return nil, actionFail, err.Error()
	}

	if existing.Mode().Type() != kind {
		return nil, actionFail, "type conflict"
	}

	if kind == fs.ModeDir || s.options.Policy == Always {
		return existing, actionRestore, ""
	}

	if s.options.Policy == Newer {
		if s.options.Committed.After(existing.ModTime()) {
			return existing, actionRestore, ""
		}

		return nil, actionSkip, "destination is newer"
	}

	return nil, actionSkip, "exists"
}

func (s *run) write(
	reader io.Reader, header *tar.Header, relative string, kind fs.FileMode, existing fs.FileInfo,
) error {
	if kind == fs.ModeDir {
		return fault.Wrap("create directory", s.root.Mkdir(relative, directoryMode))
	}

	if kind != fs.ModeSymlink {
		return s.writeRegular(reader, header, relative, existing)
	}

	if existing != nil {
		err := s.root.Remove(relative)
		if err != nil {
			return fault.Wrap("replace symlink", err)
		}
	}

	return fault.Wrap("create symlink", s.root.Symlink(header.Linkname, relative))
}

// writeRegular stages the content next to its destination and renames it into place, keeping the
// mode and owner of a replaced file.
func (s *run) writeRegular(reader io.Reader, header *tar.Header, relative string, existing fs.FileInfo) error {
	mode := fs.FileMode(fileMode)
	if existing != nil {
		// Keep replacements private until their original owner and mode have been applied.
		mode = 0o600
	} else if header.Mode&executableBits != 0 {
		mode = executableMode
	}

	temporary := filepath.Join(filepath.Dir(relative), temporaryPrefix+filepath.Base(relative))

	file, err := s.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fault.Wrap("create temporary file", err)
	}
	// A successful rename leaves nothing to remove; any earlier failure does.
	defer resource.Remove(s.root, temporary)

	err = fill(file, reader, existing)
	if err != nil {
		resource.Close(file)

		return err
	}

	err = file.Close()
	if err != nil {
		return fault.Wrap("close temporary file", err)
	}

	return fault.Wrap("replace file", s.root.Rename(temporary, relative))
}

func fill(file *os.File, reader io.Reader, existing fs.FileInfo) error {
	_, err := io.Copy(file, reader)
	if err != nil {
		return fault.Wrap("write content", err)
	}

	if existing == nil {
		return nil
	}

	stat, ok := existing.Sys().(*syscall.Stat_t)
	if ok {
		err = file.Chown(int(stat.Uid), int(stat.Gid))
		if err != nil {
			return fault.Wrap("preserve owner", err)
		}
	}

	return fault.Wrap("preserve mode", file.Chmod(existing.Mode().Perm()))
}

func (s *run) report(summary *Summary, outcome action, relative, reason string) error {
	var line string

	switch outcome {
	case actionNone:
		return nil
	case actionRestore:
		summary.Restored++
		line = "restored " + filepath.Join(s.base, relative)
	case actionSkip:
		summary.Skipped++
		line = "skipped " + filepath.Join(s.base, relative) + ": " + reason
	case actionFail:
		summary.Failed++
		line = "failed " + filepath.Join(s.base, relative) + ": " + reason
	}

	_, err := fmt.Fprintln(s.options.Output, line)

	return fault.Wrap("write restore result", err)
}
