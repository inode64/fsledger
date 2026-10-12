package restore

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"

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

func replacementFixture(t *testing.T, mode fs.FileMode) (*os.Root, fs.FileInfo) {
	t.Helper()

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(root) })

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

	return root, existing
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

			root, existing := replacementFixture(t, scenario.mode)
			reader := inspectingReader{Reader: strings.NewReader("restored"), inspect: func() {
				assertPermissions(t, root, temporaryPrefix+replacementName, 0o600)
			}}

			state := run{root: root, base: root.Name(), name: "", options: Options{}, opened: true}
			//nolint:exhaustruct_v5 // Only the executable bits are consulted when writing file content.
			err := state.writeRegular(reader, &tar.Header{Mode: 0o755}, replacementName, existing)
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
