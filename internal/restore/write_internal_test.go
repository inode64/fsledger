package restore

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/resource"
)

type inspectingReader struct {
	io.Reader

	inspect func()
}

func (r inspectingReader) Read(buffer []byte) (int, error) {
	r.inspect()

	return r.Reader.Read(buffer) //nolint:wrapcheck // Preserve io.Reader sentinel errors for io.Copy.
}

const replacementName = "secret"

func replacementFixture(t *testing.T, mode fs.FileMode) (*os.Root, int, fs.FileInfo) {
	t.Helper()

	directory := t.TempDir()

	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(root) })

	parent, err := unix.Open(directory, directoryFlags, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.FD(parent) })

	err = root.WriteFile(replacementName, []byte("original"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = root.Chmod(replacementName, mode)
	if err != nil {
		t.Fatal(err)
	}

	existing, err := root.Lstat(replacementName)
	if err != nil {
		t.Fatal(err)
	}

	return root, parent, existing
}

func assertPermissions(t *testing.T, root *os.Root, name string, want fs.FileMode) {
	t.Helper()

	info, err := root.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != want {
		t.Fatalf("%s permissions: %04o, want %04o", name, info.Mode().Perm(), want)
	}
}

func TestWriteRegularKeepsReplacementPrivateUntilCopied(t *testing.T) {
	t.Parallel()

	for _, scenario := range []struct {
		name string
		mode fs.FileMode
	}{
		{name: "private", mode: 0o600},
		{name: "group executable", mode: 0o750},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()

			root, parent, existing := replacementFixture(t, scenario.mode)
			reader := inspectingReader{Reader: strings.NewReader("restored"), inspect: func() {
				staged := temporaries(t, root)
				if len(staged) != 1 {
					t.Fatal("staged copies:", staged)
				}

				assertPermissions(t, root, staged[0], 0o600)
			}}

			//nolint:exhaustruct_v5 // Only the executable bits are consulted when writing file content.
			err := writeRegular(reader, &tar.Header{Mode: 0o755}, parent, replacementName, existing)
			if err != nil {
				t.Fatal(err)
			}

			assertPermissions(t, root, replacementName, scenario.mode)

			content, err := root.ReadFile(replacementName)
			if err != nil || string(content) != "restored" {
				t.Fatal(string(content), err)
			}
		})
	}
}

func TestEntryOffPreservesConcurrentCreation(t *testing.T) {
	t.Parallel()

	root, parent, _ := replacementFixture(t, 0o600)

	err := root.Remove(replacementName)
	if err != nil {
		t.Fatal(err)
	}

	// The destination appears only when content is read, after the overwrite policy was checked.
	reader := inspectingReader{Reader: strings.NewReader("archived"), inspect: sync.OnceFunc(func() {
		writeErr := root.WriteFile(replacementName, []byte("concurrent"), 0o600)
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	})}
	state := new(run)
	state.root = parent
	state.options.Policy = Off

	//nolint:exhaustruct_v5 // Only the entry type and executable bits are needed here.
	header := &tar.Header{Typeflag: tar.TypeReg, Mode: 0o644}

	outcome, reason := state.entry(reader, header, replacementName)
	if outcome != actionFail || reason == "" {
		t.Fatal("concurrent creation was not reported:", outcome, reason)
	}

	content, err := root.ReadFile(replacementName)
	if err != nil || string(content) != "concurrent" {
		t.Fatal("concurrent file was replaced:", string(content), err)
	}

	assertPermissions(t, root, replacementName, 0o600)

	if leftovers := temporaries(t, root); len(leftovers) != 0 {
		t.Fatal("temporary copy left behind:", leftovers)
	}
}
