package mirror_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/integrity"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/resource"
)

func TestDuplicateEventsKeepMirrorFile(t *testing.T) {
	t.Parallel()
	source, destination, syncer := cachedMirror(t)
	before := mirrorInfo(t, destination)

	for _, path := range []string{source, filepath.Dir(source)} {
		err := syncer.ApplyPaths(t.Context(), []string{path})
		if err != nil {
			t.Fatal(err)
		}

		if !os.SameFile(before, mirrorInfo(t, destination)) {
			t.Fatal("unchanged event recopied the mirror")
		}
	}
}

// A restart empties the cache; identical copies must be verified, not rewritten and synced again.
//
//nolint:cyclop,funlen,gocognit,gocyclo // One restart covers reuse and each reason to replace a copy.
func TestRestartVerifiesExistingMirrorCopies(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	repository := t.TempDir()

	for name, perm := range map[string]os.FileMode{
		"same": 0o600, "changed": 0o600, "grown": 0o600, "mode": 0o600, "link": 0o600, "inner": 0o600,
	} {
		err := os.WriteFile(filepath.Join(source, name), []byte("original"), perm)
		if err != nil {
			t.Fatal(err)
		}
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	reconcile := func() {
		t.Helper()

		syncer, openErr := mirror.Open(repository, []string{source}, matcher, "sha256")
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer resource.Close(syncer)

		openErr = syncer.ReconcileContext(t.Context())
		if openErr != nil {
			t.Fatal(openErr)
		}
	}

	reconcile()

	mirrored := func(name string) string { return filepath.Join(repository, strings.TrimPrefix(source, "/"), name) }
	before := mirrorInfo(t, mirrored("same"))

	err = os.WriteFile(filepath.Join(source, "changed"), []byte("ORIGINAL"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(source, "grown"), []byte("original and more"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Chmod(filepath.Join(source, "mode"), 0o700) //nolint:gosec // The executable bit is under test.
	if err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "outside")

	err = os.WriteFile(outside, []byte("original"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Remove(mirrored("link"))
	if err == nil {
		err = os.Symlink(outside, mirrored("link"))
	}

	// os.Root follows a relative in-root symlink even with O_NOFOLLOW; its identical target must not count.
	if err == nil {
		err = os.Remove(mirrored("inner"))
	}

	if err == nil {
		err = os.Symlink("same", mirrored("inner"))
	}

	if err != nil {
		t.Fatal(err)
	}

	reconcile()

	if !os.SameFile(before, mirrorInfo(t, mirrored("same"))) {
		t.Fatal("restart rewrote an identical mirror copy")
	}

	data, err := os.ReadFile(mirrored("changed"))
	if err != nil || string(data) != "ORIGINAL" {
		t.Fatalf("restart kept stale bytes: %q %v", data, err)
	}

	data, err = os.ReadFile(mirrored("grown"))
	if err != nil || string(data) != "original and more" {
		t.Fatalf("restart kept a shorter copy: %q %v", data, err)
	}

	if perm := mirrorInfo(t, mirrored("mode")).Mode().Perm(); perm != 0o700 {
		t.Fatalf("restart kept stale mode: %v", perm)
	}

	for _, name := range []string{"link", "inner"} {
		if !mirrorInfo(t, mirrored(name)).Mode().IsRegular() {
			t.Fatalf("restart trusted mirror symlink %s", name)
		}
	}
}

// Keeping an identical copy still reads the source once, so the catalog can reuse that digest.
func TestRestartKeptCopyOffersDigest(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	repository := t.TempDir()
	source := filepath.Join(directory, "file")

	err := os.WriteFile(source, []byte("original"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	open := func() *mirror.Sync {
		t.Helper()

		syncer, openErr := mirror.Open(repository, []string{directory}, matcher, "sha256")
		if openErr != nil {
			t.Fatal(openErr)
		}

		t.Cleanup(func() { resource.Close(syncer) })

		openErr = syncer.ReconcileContext(t.Context())
		if openErr != nil {
			t.Fatal(openErr)
		}

		return syncer
	}

	open()

	second := open()

	used := false
	lookup := func(record integrity.Record, algorithm string) (string, int64, bool) {
		digest, when, ok := second.CopiedHash(record, algorithm)
		used = ok

		return digest, when, ok
	}

	record, err := integrity.NewScanner(1, 2).Observe(t.Context(), source, "sha256", true, lookup)
	if err != nil || !used || record.Hash != fmt.Sprintf("%x", sha256.Sum256([]byte("original"))) {
		t.Fatalf("kept copy offered no digest: %+v %v %v", record, used, err)
	}
}

func cachedMirror(t *testing.T) (string, string, *mirror.Sync) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "file")

	err := os.WriteFile(source, []byte("original"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	repository := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := mirror.Open(repository, []string{filepath.Dir(source)}, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(syncer) })

	err = syncer.ApplyPaths(t.Context(), []string{source})
	if err != nil {
		t.Fatal(err)
	}

	return source, filepath.Join(repository, strings.TrimPrefix(source, "/")), syncer
}

func mirrorInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info
}

//nolint:gocognit // Ordered mutations preserve size and restore mtime on each side of the mirror.
func TestMirrorCacheDetectsSourceAndDestinationChanges(t *testing.T) {
	t.Parallel()

	for _, changeSource := range []bool{false, true} {
		t.Run(strconv.FormatBool(changeSource), func(t *testing.T) {
			t.Parallel()
			source, destination, syncer := cachedMirror(t)

			target, expected := destination, "original"
			if changeSource {
				target, expected = source, "modified"
			}

			before := mirrorInfo(t, target)

			err := os.WriteFile(target, []byte("modified"), 0o600)
			if err != nil {
				t.Fatal(err)
			}
			// Equal size and restored mtime must not hide a write to either copy.
			err = os.Chtimes(target, before.ModTime(), before.ModTime())
			if err != nil {
				t.Fatal(err)
			}

			err = syncer.ApplyPaths(t.Context(), []string{source})
			if err != nil {
				t.Fatal(err)
			}

			//nolint:gosec // Destination is the private mirror created by cachedMirror.
			data, err := os.ReadFile(destination)
			if err != nil || string(data) != expected {
				t.Fatalf("lost change: %q %v", data, err)
			}
		})
	}
}

func TestRefreshBypassesMirrorCache(t *testing.T) {
	t.Parallel()
	source, destination, syncer := cachedMirror(t)
	before := mirrorInfo(t, destination)

	err := syncer.RefreshContext(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}

	if os.SameFile(before, mirrorInfo(t, destination)) {
		t.Fatal("forced hash refresh trusted cached metadata")
	}
}

func TestCopiedHashRequiresFreshStableSource(t *testing.T) {
	t.Parallel()
	source, _, syncer := cachedMirror(t)
	scanner := integrity.NewScanner(1, 2)
	used := false
	lookup := func(record integrity.Record, algorithm string) (string, int64, bool) {
		hash, when, ok := syncer.CopiedHash(record, algorithm)
		used = ok

		return hash, when, ok
	}

	record, err := scanner.Observe(t.Context(), source, "sha256", true, lookup)
	if err != nil || !used || record.Hash != fmt.Sprintf("%x", sha256.Sum256([]byte("original"))) {
		t.Fatalf("fresh copy not reused: %+v %v %v", record, used, err)
	}

	if _, _, ok := syncer.CopiedHash(record, "blake3"); ok {
		t.Fatal("wrong algorithm accepted")
	}

	reconciled := time.Now().UnixNano()

	err = syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// The source is still racy, so reconciliation compares its bytes again and may offer that fresh digest.
	if _, hashedAt, ok := syncer.CopiedHash(record, "sha256"); ok && hashedAt < reconciled {
		t.Fatal("previous operation hash retained")
	}

	err = syncer.RefreshContext(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}

	before := mirrorInfo(t, source)

	err = os.WriteFile(source, []byte("modified"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Chtimes(source, before.ModTime(), before.ModTime())
	if err != nil {
		t.Fatal(err)
	}

	record, err = scanner.Observe(t.Context(), source, "sha256", true, lookup)
	if err != nil || used || record.Hash != fmt.Sprintf("%x", sha256.Sum256([]byte("modified"))) {
		t.Fatalf("stale copy trusted: %+v %v %v", record, used, err)
	}
}
