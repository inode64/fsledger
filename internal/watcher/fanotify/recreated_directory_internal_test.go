package fanotify

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/watcher"
)

// rm -rf conf.d && mkdir -p conf.d/nested may hand the new directories the deleted inode numbers (ext4, xfs).
// The kernel marks died with the old inodes, so the identities remembered for them must not stand for marks.
func TestRecreatedDirectoryWithReusedInodeIsMarkedAgain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	recreated := filepath.Join(root, "conf.d")
	nested := filepath.Join(recreated, "nested")

	err := os.MkdirAll(nested, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name, path, old string
		mask            uint64
		hasOld          bool
	}{
		{"delete", recreated, "", unix.FAN_DELETE, false},
		{"moved from", recreated, "", unix.FAN_MOVED_FROM, false},
		{"rename within the selection", filepath.Join(root, "renamed"), recreated, unix.FAN_RENAME, true},
		{"one-sided rename", recreated, "", unix.FAN_RENAME, true},
	} {
		detector := rememberedTree(t, root, recreated, nested)

		// The current directories carry the remembered identities, exactly as recreated ones reusing the inodes.
		detector.updateDirectoryCoverage(test.path, test.old, unix.FAN_ONDIR|test.mask, test.hasOld)
		assertRecreatedTreeUncovered(t, test.name, detector, root, recreated, nested)

		// A rename also requests marks for the name it reached; only the recreation matters here.
		for len(detector.refreshPaths) != 0 {
			<-detector.refreshPaths
		}

		detector.updateDirectoryCoverage(recreated, "", unix.FAN_ONDIR|unix.FAN_CREATE, false)

		select {
		case path := <-detector.refreshPaths:
			err = detector.markTree(path, nil)
		default:
			t.Fatal(test.name, "recreated directory did not request marks")
		}

		if err == nil {
			t.Fatal(test.name, "recreated directory reused the departed directory's mark")
		}

		assertRecreatedTreeUncovered(t, test.name, detector, root, recreated, nested)
	}
}

// rememberedTree returns a rootless watcher that remembers the current identity of each directory.
func rememberedTree(t *testing.T, root string, directories ...string) *Watcher {
	t.Helper()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)
	detector.fd = -1 // Any fanotify_mark call fails, which proves that a directory is marked again.
	detector.mode = ModeDFIDName
	detector.matcher = matcher
	detector.queue = watcher.NewQueue(1)
	detector.path = root
	detector.refreshPaths = make(chan string, 1)
	detector.refreshAll = make(chan struct{}, 1)

	for _, directory := range append([]string{root}, directories...) {
		var stat unix.Stat_t

		err = unix.Lstat(directory, &stat)
		if err != nil {
			t.Fatal(err)
		}

		// The whole fixture lives on the root's filesystem.
		detector.device = stat.Dev
		detector.rememberMark(directory, directoryIdentity{device: stat.Dev, inode: stat.Ino})
	}

	return detector
}

// assertRecreatedTreeUncovered requires companion events below the recreated tree while it is unmarked.
func assertRecreatedTreeUncovered(t *testing.T, name string, detector *Watcher, root, recreated, nested string) {
	t.Helper()

	if detector.Covers(filepath.Join(recreated, "file")) || detector.Covers(filepath.Join(nested, "file")) {
		t.Fatal(name, "unmarked recreated directory suppressed companion events")
	}

	if !detector.Covers(filepath.Join(root, "file")) || detector.Dirty() {
		t.Fatal(name, "the surviving parent lost its coverage")
	}
}
