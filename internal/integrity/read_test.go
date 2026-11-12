package integrity_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/resource"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/config"
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
		ScanPaths(t.Context(), []string{root}, nil, matcher, "sha256", true, true, func(record integrity.Record) error {
			found[filepath.Base(string(record.Path))] = record

			return nil
		}, nil)
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
		true, nil,
	)
	if err == nil {
		t.Fatal("parent symlink traversed")
	}

	record, err := integrity.NewScanner(1, 2).Observe(t.Context(), name, "sha256", true, nil)
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

	before, err := integrity.NewScanner(1, 2).Observe(t.Context(), path, "sha256", true, nil)
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

	after, err := integrity.NewScanner(1, 2).Observe(t.Context(), path, "sha256", true, nil)
	if err != nil {
		t.Fatal(err)
	}

	if before.Hash == after.Hash || before.Size != after.Size || before.Mtime != after.Mtime {
		t.Fatal("full hash did not detect replacement")
	}
}

// utimensat accepts times that Unix nanoseconds cannot hold. They are recorded as the nearest end of
// that range, so a time far in the future is not taken for one before the file's birth, and one far in
// the past still is.
func TestTimesOutsideUnixNanosecondsKeepTheirSide(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name      string
		year      int
		want      int64
		backdated bool
	}{
		{"future", 2400, math.MaxInt64, false},
		{"past", 1601, math.MinInt64, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			before, after := observedAroundYear(t, fixture.year)
			if after.Mtime != fixture.want {
				t.Fatalf("recorded mtime %d (%v), want %d", after.Mtime, time.Unix(0, after.Mtime).UTC(), fixture.want)
			}

			if !after.HasBtime {
				t.Skip("temporary directory filesystem has no birth times")
			}

			retimed := slices.Contains(integrity.Differences(before, after, config.Integrity{}), integrity.FieldMtime)
			if retimed != fixture.backdated || after.Backdated() > 0 != fixture.backdated {
				t.Fatalf("change %v, backdated by %v, want %v", retimed, after.Backdated(), fixture.backdated)
			}
		})
	}
}

// observedAroundYear observes a new file before and after its times are set to the start of year.
func observedAroundYear(t *testing.T, year int) (integrity.Record, integrity.Record) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "file")

	err := os.WriteFile(path, []byte("content"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	scanner := integrity.NewScanner(1, 2)

	before, err := scanner.Observe(t.Context(), path, hashSHA256, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	// os.Chtimes goes through Unix nanoseconds itself, so the time is handed to the kernel in seconds.
	set := unix.Timespec{Sec: time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC).Unix(), Nsec: 0}

	err = unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{set, set}, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		t.Fatal(err)
	}

	var kept unix.Stat_t

	err = unix.Lstat(path, &kept)
	if err != nil {
		t.Fatal(err)
	}

	if kept.Mtim != set {
		t.Skipf("temporary directory filesystem keeps the year %d as second %d", year, kept.Mtim.Sec)
	}

	after, err := scanner.Observe(t.Context(), path, hashSHA256, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	return before, after
}
