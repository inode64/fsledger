package pathutil_test

import (
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/pathutil"
)

func TestValidRelative(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"file", "dir/file", "dir/name with spaces", "dir/line\nbreak", "-option"} {
		if !pathutil.ValidRelative(path) {
			t.Errorf("rejected local entry %q", path)
		}
	}

	for _, path := range []string{
		"", ".", "..", "/file", "../file", "dir/../file", "./file", "dir//file", "dir/", "dir/\x00file",
	} {
		if pathutil.ValidRelative(path) {
			t.Errorf("accepted unsafe or noncanonical entry %q", path)
		}
	}
}

func TestCompactRoots(t *testing.T) {
	const child = "/a/b"

	t.Parallel()

	for _, test := range []struct{ paths, want []string }{
		{[]string{"/a/b/c", "/a-b", "/a", "/ab", child, "/a"}, []string{"/a", "/a-b", "/ab"}},
		{[]string{child, "/a/c"}, []string{child, "/a/c"}},
		{[]string{"/", "/a", "/a/b"}, []string{"/"}},
		{nil, nil},
	} {
		if got := pathutil.CompactRoots(test.paths); !slices.Equal(got, test.want) {
			t.Fatalf("%q: got %q want %q", test.paths, got, test.want)
		}
	}
}
