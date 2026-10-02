// Package inbound applies explicitly enabled incoming Git changes to source paths.
package inbound

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
)

// ErrConflict means the incoming operation cannot safely overwrite current sources.
var ErrConflict = errors.New("incoming synchronization conflict")

const (
	absent     = "000000"
	regular    = "100644"
	executable = "100755"
	symlink    = "120000"
	maxChanges = 4096
)

// Objects exposes only immutable blob reads; Git and its subprocesses stay in their adapter.
type Objects interface {
	ReadBlob(ctx context.Context, identifier string) ([]byte, error)
}

// Version identifies one immutable Git entry, or its absence.
type Version struct {
	Mode   string `json:"mode"`
	Object string `json:"object"`
}

// Change retains raw Linux filename bytes when persisted as JSON.
type Change struct {
	Before Version `json:"before"`
	After  Version `json:"after"`
	Path   []byte  `json:"path"`
}

// Root pins an existing directory (or a file source's parent) across recovery.
type Root struct {
	Path   []byte `json:"path"`
	Anchor []byte `json:"anchor"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Mount  uint64 `json:"mount"`
}

// Plan is a durable journal of identities and progress, never file contents.
type Plan struct {
	ID       string   `json:"id"`
	Base     string   `json:"base"`
	Target   string   `json:"target"`
	Policy   string   `json:"policy"`
	Conflict string   `json:"conflict,omitempty"`
	Roots    []Root   `json:"roots"`
	Changes  []Change `json:"changes"`
	Done     int      `json:"done"`
}

// Prepare validates the entire change set before the first source mutation.
func Prepare(ctx context.Context, objects Objects, roots []string, matcher *exclude.Matcher,
	base, target, policy string, differences map[string]string,
) (*Plan, error) {
	if len(differences) > maxChanges {
		return nil, conflict("incoming commit exceeds 4096 changed paths")
	}

	plan := &Plan{
		ID: rand.Text(), Base: base, Target: target, Policy: policy,
		Conflict: "", Roots: nil, Changes: nil, Done: 0,
	}

	for _, path := range roots {
		root, err := pinRoot(path)
		if err != nil {
			return nil, err
		}

		plan.Roots = append(plan.Roots, root)
	}

	for path, header := range differences {
		change, err := parseChange(path, header)
		if err != nil {
			return nil, err
		}

		if !pathutil.Within(roots, path) || matcher.Match(path) {
			return nil, conflict("incoming path is outside selected sources: " + path)
		}

		plan.Changes = append(plan.Changes, change)
	}

	// Writes precede deletions; type changes involving directories fail preflight.
	slices.SortFunc(plan.Changes, compareChanges)

	for _, change := range plan.Changes {
		err := preflight(ctx, objects, change)
		if err != nil {
			return nil, pathError(err)
		}
	}

	return plan, nil
}

func parseChange(path, header string) (Change, error) {
	const fieldsPerChange = 5

	fields := strings.Fields(header)
	if len(fields) != fieldsPerChange || !pathutil.ValidAbsolute(path) || path == "/" {
		return Change{}, conflict("invalid incoming tree change")
	}

	before, after := strings.TrimPrefix(fields[0], ":"), fields[1]
	for _, mode := range []string{before, after} {
		if mode != absent && mode != regular && mode != executable && mode != symlink {
			return Change{}, conflict("unsupported incoming Git entry type: " + path)
		}
	}

	return Change{
		Path: []byte(path), Before: Version{Mode: before, Object: fields[2]},
		After: Version{Mode: after, Object: fields[3]},
	}, nil
}

func compareChanges(first, second Change) int {
	if (first.After.Mode == absent) != (second.After.Mode == absent) {
		if first.After.Mode == absent {
			return 1
		}

		return -1
	}

	return bytes.Compare(first.Path, second.Path)
}

func pinRoot(path string) (Root, error) {
	stat, err := statPath(path)
	if err != nil {
		return Root{}, err
	}

	anchor := path
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		anchor = filepath.Dir(path)
	}

	parent, name, err := integrity.OpenParent(anchor)
	if err != nil {
		return Root{}, err
	}
	defer resource.FD(parent)

	var info unix.Statx_t

	err = unix.Statx(parent, name, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_INO|unix.STATX_MNT_ID, &info)
	if err != nil || info.Mask&unix.STATX_MNT_ID == 0 {
		return Root{}, conflict("incoming synchronization requires filesystem mount identity: " + path)
	}

	device := unix.Mkdev(info.Dev_major, info.Dev_minor)

	return Root{Path: []byte(path), Anchor: []byte(anchor), Device: device, Inode: info.Ino, Mount: info.Mnt_id}, nil
}

// CheckRoots rejects disappeared, replaced or remounted write destinations.
func (plan *Plan) CheckRoots() error {
	for _, expected := range plan.Roots {
		actual, err := pinRoot(string(expected.Anchor))
		if err != nil {
			return err
		}

		if expected.Device != actual.Device || expected.Inode != actual.Inode || expected.Mount != actual.Mount {
			return conflict("incoming destination identity changed: " + string(expected.Path))
		}
	}

	return nil
}

func conflict(message string) error { return fmt.Errorf("%w: %s", ErrConflict, message) }

func statPath(path string) (unix.Stat_t, error) {
	parent, name, err := integrity.OpenParent(path)
	if err != nil {
		return unix.Stat_t{}, err
	}
	defer resource.FD(parent)

	var stat unix.Stat_t

	err = unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)

	return stat, fault.Wrap("inspect incoming destination", err)
}
