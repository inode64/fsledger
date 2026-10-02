package pathindex_test

import (
	"reflect"
	"testing"

	"github.com/inode64/fsledger/internal/pathindex"
)

func TestSubtreesRespectComponentsAndArbitraryBytes(t *testing.T) {
	t.Parallel()

	var index pathindex.Index[int]
	for path, value := range map[string]int{"/": 1, "/a": 2, "/ab": 3, "/a/x": 4, "/a/x/\xff": 5} {
		index.Set(path, value)
	}

	want := map[string]int{"/": 1, "/a": 2, "/a/x": 4, "/a/x/\xff": 5}
	if got := index.Overlaps("/a/x"); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}

	index.DeleteSubtree("/a")

	if got := index.Subtree("/"); !reflect.DeepEqual(got, map[string]int{"/": 1, "/ab": 3}) {
		t.Fatal(got)
	}

	index.Set("/a", 9)

	if value, ok := index.Get("/a"); !ok || value != 9 {
		t.Fatal(value, ok)
	}
}
