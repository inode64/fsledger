package filehandle_test

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/filehandle"
	"github.com/inode64/fsledger/internal/resource"
)

func handleOf(t *testing.T, path string) filehandle.Handle {
	t.Helper()

	handle, _, err := unix.NameToHandleAt(unix.AT_FDCWD, path, 0)
	if err != nil {
		t.Skip("filesystem handles unavailable:", err)
	}

	var stat unix.Statfs_t

	err = unix.Statfs(path, &stat)
	if err != nil {
		t.Fatal(err)
	}

	value := filehandle.Handle{Bytes: handle.Bytes(), Type: handle.Type(), FSID: [8]byte{}}
	//nolint:gosec // Preserve the signed fsid ABI bit pattern, not a numerical conversion.
	binary.NativeEndian.PutUint32(value.FSID[:4], uint32(stat.Fsid.Val[0]))
	//nolint:gosec // Preserve the signed fsid ABI bit pattern, not a numerical conversion.
	binary.NativeEndian.PutUint32(value.FSID[4:], uint32(stat.Fsid.Val[1]))

	return value
}

func TestDeletedSuffixInRealNameIsNotDeletion(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	resolver, err := filehandle.Open(root)
	if err != nil {
		t.Skip("handle resolution requires CAP_DAC_READ_SEARCH:", err)
	}

	t.Cleanup(func() { resource.Close(resolver) })

	named := filepath.Join(root, "backup (deleted)")
	removed := filepath.Join(root, "removed")

	for _, path := range []string{named, removed} {
		err = os.Mkdir(path, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := resolver.Path(handleOf(t, named))
	if err != nil || got != named {
		t.Fatalf("real name treated as deleted: %q %v", got, err)
	}

	// Hold the inode so the handle outlives the name and procfs reports the suffix.
	//nolint:gosec // The directory lives in this test's private temporary root.
	held, err := os.Open(removed)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(held) })

	handle := handleOf(t, removed)

	err = os.Remove(removed)
	if err != nil {
		t.Fatal(err)
	}

	_, err = resolver.Path(handle)
	if !errors.Is(err, filehandle.ErrDeleted) && !errors.Is(err, unix.ESTALE) {
		t.Fatalf("deleted directory resolved: %v", err)
	}
}
