package restore

import (
	"archive/tar"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/resource"
)

// temporaries lists the staged copies in the fixture directory.
func temporaries(t *testing.T, root *os.Root) []string {
	t.Helper()

	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		t.Fatal(err)
	}

	var names []string

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), temporaryPrefix) {
			names = append(names, entry.Name())
		}
	}

	return names
}

func assertRestored(t *testing.T, root *os.Root, parent int, name string) {
	t.Helper()

	//nolint:exhaustruct_v5 // Only the executable bits are consulted when writing file content.
	err := writeRegular(strings.NewReader("restored"), &tar.Header{Mode: 0o644}, parent, name, nil)
	if err != nil {
		t.Fatal(err)
	}

	content, err := root.ReadFile(name)
	if err != nil || string(content) != "restored" {
		t.Fatal(string(content), err)
	}
}

func TestWriteRegularIgnoresStaleTemporary(t *testing.T) {
	t.Parallel()

	const name = "restored"

	root, parent, _ := replacementFixture(t, 0o600)

	// A copy left by a crash, or planted by another writer, under the name once derived from the destination.
	const stale = temporaryPrefix + name

	err := root.WriteFile(stale, []byte("stale"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	assertRestored(t, root, parent, name)

	content, err := root.ReadFile(stale)
	if err != nil || string(content) != "stale" {
		t.Fatal("stale copy was touched:", string(content), err)
	}

	leftovers := temporaries(t, root)
	if len(leftovers) != 1 {
		t.Fatal("temporary copy left behind:", leftovers)
	}
}

func TestWriteRegularAcceptsLongestName(t *testing.T) {
	t.Parallel()

	// NAME_MAX on Linux; any prefix added to such a name is refused with ENAMETOOLONG.
	const nameMax = 255

	root, parent, _ := replacementFixture(t, 0o600)

	assertRestored(t, root, parent, strings.Repeat("n", nameMax))

	leftovers := temporaries(t, root)
	if len(leftovers) != 0 {
		t.Fatal("temporary copy left behind:", leftovers)
	}
}

func TestCreateTemporaryRetriesTakenName(t *testing.T) {
	t.Parallel()

	const fresh = "fresh"

	root, parent, _ := replacementFixture(t, 0o600)
	names := []string{replacementName, fresh}

	file, err := createTemporary(parent, 0o600, func() string {
		name := names[0]
		names = names[1:]

		return name
	})
	if err != nil {
		t.Fatal(err)
	}

	resource.Close(file)

	if file.Name() != fresh {
		t.Fatal("temporary name:", file.Name())
	}

	content, err := root.ReadFile(replacementName)
	if err != nil || string(content) != "original" {
		t.Fatal("taken name was touched:", string(content), err)
	}
}

func TestCreateTemporaryGivesUpOnTakenNames(t *testing.T) {
	t.Parallel()

	_, parent, _ := replacementFixture(t, 0o600)

	_, err := createTemporary(parent, 0o600, func() string { return replacementName })
	if !errors.Is(err, fs.ErrExist) {
		t.Fatal("taken names were not reported:", err)
	}
}
