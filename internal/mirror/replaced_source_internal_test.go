package mirror

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

// replacedRegular stats a regular source and then replaces it, as a change between the stat in copy and its open.
func replacedRegular(t *testing.T, source string, replace func(string) error) (int, string, unix.Stat_t) {
	t.Helper()
	writeUnstableFixture(t, source, "regular")

	parent, name, err := integrity.OpenParent(source)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.FD(parent) })

	var stat unix.Stat_t

	err = unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		t.Fatal(err)
	}

	err = errors.Join(os.Remove(source), replace(source))
	if err != nil {
		t.Fatal(err)
	}

	return parent, name, stat
}

func TestCopyRetriesRegularFileReplacedBeforeOpening(t *testing.T) {
	t.Parallel()

	replacements := map[string]func(string) error{
		"symlink": func(path string) error { return os.Symlink("elsewhere", path) },
		"socket":  func(path string) error { return unix.Mknod(path, unix.S_IFSOCK|0o600, 0) },
	}
	for kind, replace := range replacements {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			source := filepath.Join(root, "regular")

			matcher, err := exclude.New()
			if err != nil {
				t.Fatal(err)
			}

			syncer, err := Open(t.TempDir(), []string{root}, matcher, "")
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { resource.Close(syncer) })

			parent, name, stat := replacedRegular(t, source, replace)

			relative, err := Relative(source)
			if err != nil {
				t.Fatal(err)
			}

			err = syncer.copyRegular(t.Context(), parent, name, source, relative, stat, copyForced)
			if !errors.Is(err, integrity.ErrUnstable) {
				t.Fatal("replaced source was not retried", err)
			}
		})
	}
}
