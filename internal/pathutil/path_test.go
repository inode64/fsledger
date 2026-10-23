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

func TestHasTemporaryComponent(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"/srv/.fsledger-inbound-ID-0", "/srv/.fsledger-restore-ABC", "srv/.fsledger-restore-ABC/file",
		".fsledger-inbound-",
	} {
		if !pathutil.HasTemporaryComponent(path) {
			t.Errorf("missed fsledger temporary %q", path)
		}
	}

	for _, path := range []string{
		"/srv/.FSLEDGER-RESTORE-ABC", "/srv/fsledger-restore-ABC", "/srv/x.fsledger-inbound-ID-0",
		"/srv/.fsledger-restored", "/srv/.fsledger-inbound", "/srv/.fsledger-other-ABC", "/srv/.fsledger",
	} {
		if pathutil.HasTemporaryComponent(path) {
			t.Errorf("reserved ordinary name %q", path)
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

func TestContainsComparesComponents(t *testing.T) {
	t.Parallel()

	const (
		srv = "/srv"
		www = srv + "/www"
	)

	for _, testCase := range []struct {
		parent, child string
		want          bool
	}{
		{srv, srv, true},
		{srv, www, true},
		{srv + "/", www, true},
		{"/", srv, true},
		{srv, srv + "x", false},
		{www, srv, false},
		{"", srv, false},
		{"", "", false},
	} {
		if got := pathutil.Contains(testCase.parent, testCase.child); got != testCase.want {
			t.Errorf("Contains(%q, %q) = %v", testCase.parent, testCase.child, got)
		}
	}
}
