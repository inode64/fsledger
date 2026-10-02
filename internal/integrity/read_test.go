package integrity_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/resource"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

//nolint:cyclop,funlen,gocognit,gocyclo // Ordered integration fixture keeps setup, transitions and assertions together.
func TestFileTypesAndMetadata(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	file := filepath.Join(root, "file")
	//nolint:gosec // This fixture explicitly verifies group-readable mode capture.
	err := os.WriteFile(file, []byte("content\x00bytes"), 0o640)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink("/does/not/exist", filepath.Join(root, "link"))
	if err != nil {
		t.Fatal(err)
	}

	err = unix.Mkfifo(filepath.Join(root, "pipe"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := new(net.ListenConfig).Listen(t.Context(), "unix", filepath.Join(root, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(listener)

	attrErr := unix.Setxattr(file, "user.fsledger", []byte{0, 1, 255}, 0)

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	found := make(map[string]integrity.Record)

	err = integrity.NewScanner(2, 2).
		ScanPaths(t.Context(), []string{root}, nil, matcher, "sha256", true, func(record integrity.Record) error {
			found[filepath.Base(string(record.Path))] = record

			return nil
		})
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{"file": "regular", "link": "symlink", "pipe": "fifo", "socket": "socket"} {
		if found[name].Type != want {
			t.Fatalf("%s: %+v", name, found[name])
		}
	}

	hash := sha256.Sum256([]byte("content\x00bytes"))
	if found["file"].Hash != hex.EncodeToString(hash[:]) || int64(found["file"].UID) != int64(os.Getuid()) ||
		found["file"].Mode&0o777 != 0o640 {
		t.Fatal(found["file"])
	}

	if attrErr == nil &&
		(len(found["file"].Xattrs) != 1 || !bytes.Equal(found["file"].Xattrs[0].Value, []byte{0, 1, 255})) {
		t.Fatal("xattrs lost")
	}

	if string(found["link"].Target) != "/does/not/exist" || found["link"].Hash != "" {
		t.Fatal("symlink dereferenced")
	}
}

func TestParentSymlinksAndRawNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	dir := filepath.Join(root, "real")

	err := os.Mkdir(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	name := dir + "/bad-" + string([]byte{255})

	err = os.WriteFile(name, []byte("value"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink(dir, filepath.Join(root, "alias"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = integrity.NewScanner(1, 2).Observe(
		t.Context(),
		filepath.Join(root, "alias", filepath.Base(name)),
		"sha256",
		true,
	)
	if err == nil {
		t.Fatal("parent symlink traversed")
	}

	record, err := integrity.NewScanner(1, 2).Observe(t.Context(), name, "sha256", true)
	if err != nil || !bytes.Equal(record.Path, []byte(name)) {
		t.Fatalf("%v %+v", err, record)
	}
}

//nolint:gocognit // Ordered integration fixture keeps setup, transitions and assertions together.
func TestGlobalBudgetAndCancellation(t *testing.T) {
	t.Parallel()

	scanner := integrity.NewScanner(2, 2)

	var current, peak atomic.Int32

	entered := make(chan struct{}, 2)
	release := make(chan struct{})

	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			err := scanner.Do(t.Context(), func() error {
				count := current.Add(1)
				for previous := peak.Load(); count > previous; previous = peak.Load() {
					if peak.CompareAndSwap(previous, count) {
						break
					}
				}

				entered <- struct{}{}

				<-release
				current.Add(-1)

				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}

	<-entered
	<-entered

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := scanner.Do(ctx, func() error { return nil })
	if err == nil {
		t.Fatal("cancelled acquisition succeeded")
	}

	close(release)

	go func() { workers.Wait(); close(entered) }()

	drained := 0
	for range entered {
		drained++
	}

	if drained != 6 {
		t.Fatalf("expected six remaining workers, got %d", drained)
	}

	if peak.Load() != 2 {
		t.Fatal(peak.Load())
	}
}

func TestHashIgnoresRestoredMtime(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "file")

	err := os.WriteFile(path, []byte("before"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	before, err := integrity.NewScanner(1, 2).Observe(t.Context(), path, "sha256", true)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte("after!"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	modified := time.Unix(0, before.Mtime)

	err = os.Chtimes(path, modified, modified)
	if err != nil {
		t.Fatal(err)
	}

	after, err := integrity.NewScanner(1, 2).Observe(t.Context(), path, "sha256", true)
	if err != nil {
		t.Fatal(err)
	}

	if before.Hash == after.Hash || before.Size != after.Size || before.Mtime != after.Mtime {
		t.Fatal("full hash did not detect replacement")
	}
}
