package integrity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
)

// replacedRegular stats a regular source and then replaces it, as a change between the caller's stat
// and the open for hashing.
func replacedRegular(t *testing.T, path string, replace func(string) error) (int, string, unix.Stat_t) {
	t.Helper()

	err := os.WriteFile(path, []byte("regular"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	parent, name, err := OpenParent(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.FD(parent) })

	var stat unix.Stat_t

	err = unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		t.Fatal(err)
	}

	err = errors.Join(os.Remove(path), replace(path))
	if err != nil {
		t.Fatal(err)
	}

	return parent, name, stat
}

func TestHashRetriesRegularFileReplacedBeforeOpening(t *testing.T) {
	t.Parallel()

	replacements := map[string]func(string) error{
		"symlink": func(path string) error { return os.Symlink("elsewhere", path) },
		"socket":  func(path string) error { return unix.Mknod(path, unix.S_IFSOCK|0o600, 0) },
	}
	for kind, replace := range replacements {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "regular")
			parent, name, before := replacedRegular(t, path, replace)
			record := fromStat(path, &before)

			err := (&readWorkspace{}).hashFile(t.Context(), parent, name, "sha256", &before, &record)
			if !errors.Is(err, ErrUnstable) || record.Hash != "" {
				t.Fatalf("replaced source was not retried: %q %v", record.Hash, err)
			}
		})
	}
}

// replacedParent opens a directory, as the walk does before listing it, and then replaces its name;
// the open directory keeps its entries under the name with .old appended.
func replacedParent(t *testing.T, replace func(string) error) (*os.File, string) {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "parent")

	err := os.MkdirAll(filepath.Join(parent, "directory"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(parent, "regular"), []byte("regular"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	directory, err := os.Open(parent) //nolint:gosec // The fixture lives in the test's private directory.
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(directory) })

	err = errors.Join(os.Rename(parent, parent+".old"), replace(parent))
	if err != nil {
		t.Fatal(err)
	}

	return directory, parent
}

// consumeQueued observes every queued path and visits an earlier observation below parent, which the
// visitor observes again as catalog preparation does; it reports whether the consumer cancelled the scan.
func consumeQueued(ctx context.Context, scanner *Scanner, parent string, jobs <-chan string) (bool, error) {
	results := make(chan observation, cap(jobs)+1)
	for path := range jobs {
		record, err := scanner.Observe(ctx, path, "sha256", false, nil)
		results <- observation{record: record, err: err}
	}

	results <- observation{record: Record{Path: []byte(filepath.Join(parent, "regular"))}, err: nil}

	close(results)

	cancelled := false
	visit := func(record Record) error {
		_, err := scanner.Observe(ctx, string(record.Path), "sha256", false, nil)

		return err
	}

	err := consume(results, visit, func() { cancelled = true })

	return cancelled, err
}

func TestScanSkipsEntriesBelowParentReplacedAfterListing(t *testing.T) {
	t.Parallel()

	replacements := map[string]func(string) error{
		"file":    func(path string) error { return os.WriteFile(path, []byte("file"), 0o600) },
		"symlink": func(path string) error { return os.Symlink(path+".old", path) },
	}
	for kind, replace := range replacements {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			matcher, err := exclude.New()
			if err != nil {
				t.Fatal(err)
			}

			scanner := NewScanner(1, 2)
			directory, parent := replacedParent(t, replace)
			jobs := make(chan string, 2)
			walk := scanWalk{matcher: matcher, jobs: jobs, directories: true}

			err = scanner.walkChildren(t.Context(), directory, parent, walk)
			if err != nil {
				t.Fatal("listed entry below a replaced parent failed the walk", err)
			}

			close(jobs)

			cancelled, err := consumeQueued(t.Context(), scanner, parent, jobs)
			if err != nil || cancelled {
				t.Fatal("entries below a replaced parent were not gone", err, cancelled)
			}

			// A requested path below a parent that is no longer a directory remains an error.
			walk.jobs = make(chan string, 1)

			err = scanner.walk(t.Context(), filepath.Join(parent, "directory"), false, walk)
			if !errors.Is(err, unix.ENOTDIR) {
				t.Fatal("requested path below a replaced parent accepted", err)
			}
		})
	}
}
