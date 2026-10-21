package integrity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
)

const (
	// oversizedNames exceed the 64 KiB name list listxattr returns at once, so it fails with E2BIG.
	oversizedNames = 400
	// oversizedValues of the largest value size exceed maxAttributesBytes together.
	oversizedValues    = 20
	largestValue       = 64 << 10
	longAttributeLabel = 200
)

func TestAttributeReadReportsE2BIGAsTooLarge(t *testing.T) {
	t.Parallel()

	if !errors.Is(attributeReadError(-1, maxAttributesBytes, unix.E2BIG), errAttributesTooLarge) {
		t.Fatal("E2BIG not reported as oversized attributes")
	}
}

// Any owner may set user.* attributes beyond the observation limit; the entry records them as too
// large and the scan completes, so other entries, and deletions, are still reconciled.
func TestOversizedAttributesDoNotFailScans(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name, label string
		value       []byte
		count       int
	}{
		{"names", strings.Repeat("a", longAttributeLabel), nil, oversizedNames},
		{"values", "v", make([]byte, largestValue), oversizedValues},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			oversized, other := filepath.Join(root, "oversized"), filepath.Join(root, "other")

			writeEntries(t, oversized, other)
			setAttributes(t, oversized, fixture.label, fixture.value, fixture.count)

			found := scannedRecords(t, root)
			if record := found[oversized]; record.AttributesStatus != AttributesTooLarge ||
				record.Xattrs != nil || record.ACL != nil {
				t.Fatalf("oversized attributes = %q with %d xattrs", record.AttributesStatus, len(record.Xattrs))
			}

			if record, ok := found[other]; !ok || record.AttributesStatus != AttributesAvailable {
				t.Fatalf("scan did not complete: %v", found)
			}
		})
	}
}

func writeEntries(t *testing.T, paths ...string) {
	t.Helper()

	for _, path := range paths {
		err := os.WriteFile(path, []byte("x"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func setAttributes(t *testing.T, path, label string, value []byte, count int) {
	t.Helper()

	for index := range count {
		err := unix.Lsetxattr(path, fmt.Sprintf("user.%s%04d", label, index), value, 0)
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSPC) {
			t.Skip("temporary filesystem cannot hold these xattrs:", err)
		}

		if err != nil {
			t.Fatal(err)
		}
	}
}

func scannedRecords(t *testing.T, root string) map[string]Record {
	t.Helper()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]Record{}

	err = NewScanner(2, 2).ScanPaths(t.Context(), []string{root}, nil, matcher, "sha256", true, true,
		func(record Record) error {
			found[string(record.Path)] = record

			return nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}

	return found
}
