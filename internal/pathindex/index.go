// Package pathindex indexes canonical paths without scanning unrelated subtrees.
package pathindex

import (
	"cmp"
	"path/filepath"
	"strings"

	"github.com/RaduBerinde/btreemap"
)

const degree = 16

// Index is owned by one serialized worker. Its zero value is ready to use.
type Index[V any] struct{ tree *btreemap.BTreeMap[string, V] }

// Set stores the value for a canonical absolute path.
func (index *Index[V]) Set(path string, value V) {
	if index.tree == nil {
		index.tree = btreemap.New[string, V](degree, cmp.Compare[string])
	}

	index.tree.ReplaceOrInsert(path, value)
}

// Get finds an exact path.
//
//nolint:ireturn // V is the caller's concrete value type; the adapter exposes no library interface.
func (index *Index[V]) Get(path string) (V, bool) {
	if index.tree == nil {
		var zero V

		return zero, false
	}

	_, value, found := index.tree.Get(path)

	return value, found
}

// Delete removes an exact path.
func (index *Index[V]) Delete(path string) {
	if index.tree != nil {
		index.tree.Delete(path)
	}
}

// Subtree returns a stable snapshot of a path and its indexed descendants.
func (index *Index[V]) Subtree(path string) map[string]V {
	result := make(map[string]V)
	if index.tree == nil {
		return result
	}

	if value, found := index.Get(path); found {
		result[path] = value
	}

	prefix := strings.TrimSuffix(path, "/") + "/"
	for key, value := range index.tree.Ascend(btreemap.GE(prefix), btreemap.Max[string]()) {
		if !strings.HasPrefix(key, prefix) {
			break
		}

		result[key] = value
	}

	return result
}

// Overlaps returns the subtree and indexed ancestors, respecting path components.
func (index *Index[V]) Overlaps(path string) map[string]V {
	result := index.Subtree(path)
	for parent := filepath.Dir(path); parent != path; parent = filepath.Dir(parent) {
		if value, found := index.Get(parent); found {
			result[parent] = value
		}

		if parent == "/" {
			break
		}
	}

	return result
}

// DeleteSubtree removes a path and descendants without inspecting unrelated keys.
func (index *Index[V]) DeleteSubtree(path string) {
	if index.tree == nil {
		return
	}

	index.Delete(path)

	prefix := strings.TrimSuffix(path, "/") + "/"
	for {
		key, _, found := index.tree.SeekGE(prefix)
		if !found || !strings.HasPrefix(key, prefix) {
			return
		}

		index.Delete(key)
	}
}
