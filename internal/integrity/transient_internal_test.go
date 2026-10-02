package integrity

import (
	"crypto/sha256"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
)

func TestConsumeContinuesPastTransientPaths(t *testing.T) {
	t.Parallel()

	for _, failure := range []error{os.ErrNotExist, ErrUnstable} {
		results := make(chan observation, 2)
		results <- observation{record: Record{}, err: failure}

		results <- observation{record: Record{Path: []byte("/stable")}, err: nil}

		close(results)

		visited, cancelled := false, false
		err := consume(
			results,
			func(record Record) error {
				visited = string(record.Path) == "/stable"

				return nil
			},
			func() { cancelled = true },
		)

		if !visited || cancelled {
			t.Fatal("local read race cancelled unrelated observations")
		}

		if errors.Is(failure, ErrUnstable) != errors.Is(err, ErrUnstable) {
			t.Fatal("unstable observation was silently approved", err)
		}
	}
}

type changingHash struct {
	hash.Hash

	err    error
	path   string
	writes int
}

func (digest *changingHash) Write(data []byte) (int, error) {
	digest.writes++
	digest.err = os.WriteFile(digest.path, []byte("modified"), 0o600)

	written, err := digest.Hash.Write(data)

	return written, fault.Wrap("test digest", err)
}

func TestScannerRetriesUnstableContentWithoutPublishingIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "busy")

	err := os.WriteFile(path, []byte("original"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	scanner := NewScanner(1, 2)
	workspace := <-scanner.slots
	digest := &changingHash{Hash: sha256.New(), path: path, writes: 0, err: nil}

	workspace.digest, workspace.algorithm = digest, "sha256"
	scanner.slots <- workspace

	record, err := scanner.Observe(t.Context(), path, "sha256", true)
	if !errors.Is(err, ErrUnstable) || record.Hash != "" || digest.writes != 3 || digest.err != nil {
		t.Fatalf("unstable bytes accepted or unbounded retries: %+v %d %v %v", record, digest.writes, err, digest.err)
	}
}

func TestRootGuardDistinguishesFileDeletionAndLostDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "file")

	err := os.WriteFile(path, []byte("data"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	guard := RootGuard{}

	err = guard.Check([]string{root, path})
	if err != nil {
		t.Fatal(err)
	}

	err = os.Remove(path)
	if err != nil {
		t.Fatal(err)
	}

	err = guard.Check([]string{path})
	if err != nil {
		t.Fatal("configured file deletion rejected", err)
	}

	err = os.Rename(root, root+".away")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		restoreErr := os.Rename(root+".away", root)
		if restoreErr != nil {
			t.Error(restoreErr)
		}
	})

	err = guard.Check([]string{root})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing root became empty tree", err)
	}
}

func TestRootGuardRejectsChangedMountIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	guard := RootGuard{}

	err := guard.Check([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a different pinned mount ID; this is not a privileged unmount test.
	identity := guard.roots[root]
	identity.mount++

	guard.roots[root] = identity

	err = guard.Check([]string{root})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("mount replacement accepted", err)
	}
}

func TestAttributeReadRejectsChangedSizes(t *testing.T) {
	t.Parallel()

	for _, result := range []struct {
		err      error
		count    int
		capacity int
	}{
		{count: 3, capacity: 1, err: nil},
		{count: -1, capacity: 1, err: nil},
		{count: 0, capacity: 1, err: unix.ERANGE},
		{count: 0, capacity: 1, err: unix.ENODATA},
	} {
		if !errors.Is(attributeReadError(result.count, result.capacity, result.err), ErrUnstable) {
			t.Fatal("raced attribute accepted", result)
		}
	}

	if attributeReadError(0, 1, nil) != nil || attributeReadError(1, 1, nil) != nil {
		t.Fatal("valid attribute rejected")
	}

	if !errors.Is(attributeReadError(-1, 1, unix.EACCES), unix.EACCES) {
		t.Fatal("permission error hidden")
	}
}

//nolint:gocognit,funlen // Ordered live xattr mutation and bounded concurrent observation verify both syscall races.
func TestConcurrentAttributeGrowth(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "attributes")

	err := os.WriteFile(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	const name = "user.fsledger-test"

	err = unix.Lsetxattr(path, name, nil, 0)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			t.Skip("temporary filesystem lacks xattrs")
		}

		t.Fatal(err)
	}

	stop, done := make(chan struct{}), make(chan error, 1)

	go func() {
		for {
			select {
			case <-stop:
				done <- nil

				return
			default:
			}

			for _, value := range [][]byte{nil, make([]byte, 128)} {
				err := unix.Lsetxattr(path, name, value, 0)
				if err != nil {
					done <- err

					return
				}
			}

			err := unix.Lremovexattr(path, name)
			if err != nil {
				done <- err

				return
			}
		}
	}()

	defer func() {
		close(stop)

		err := <-done
		if err != nil {
			t.Error(err)
		}
	}()

	for range 2000 {
		var record Record

		err := readAttributes(path, &record)
		if err != nil && !errors.Is(err, ErrUnstable) {
			t.Fatal(err)
		}
	}
}
