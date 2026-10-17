package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

const (
	untrackedIndex = "site/index.php"
	untrackedLogo  = "site/uploads/logo.png"
)

func writeTree(t *testing.T, root string, names ...string) {
	t.Helper()

	for _, name := range names {
		path := filepath.Join(root, name)

		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(path, []byte("<?php"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// untrackedFixture records a web tree while directories are still tracked, as an existing catalog would.
func untrackedFixture(t *testing.T) (*Store, *integrity.Scanner, *exclude.Matcher, string) {
	t.Helper()

	store := testStore(t)
	store.Policy.Reference = config.ReferencePrevious
	root := t.TempDir()
	writeTree(t, root, untrackedIndex, untrackedLogo)

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	scanner := integrity.NewScanner(1, 2)

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "initial", "")
	if err != nil {
		t.Fatal(err)
	}

	announced(t, store)

	return store, scanner, matcher, root
}

// announced acknowledges every due delivery and returns the announced paths with their change kind.
func announced(t *testing.T, store *Store) map[string]string {
	t.Helper()

	deliveries, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	paths := make(map[string]string)
	identifiers := make([]int64, 0, len(deliveries))

	for _, delivery := range deliveries {
		identifiers = append(identifiers, delivery.ID)

		for _, detail := range delivery.Message.Details {
			paths[string(detail.Path)] = detail.Kind
		}
	}

	if len(identifiers) > 0 {
		err = store.DeliveredMany(t.Context(), identifiers)
		if err != nil {
			t.Fatal(err)
		}
	}

	return paths
}

func entries(t *testing.T, store *Store) int64 {
	t.Helper()

	stats, err := store.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	return stats.Entries
}

func TestUntrackedDirectoriesLeaveInventorySilently(t *testing.T) {
	t.Parallel()

	store, scanner, matcher, root := untrackedFixture(t)
	if got := entries(t, store); got != 5 {
		t.Fatalf("tracked inventory has %d entries, want root, 2 directories and 2 files", got)
	}

	store.Policy.Directories = false

	_, err := store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "periodic", "")
	if err != nil {
		t.Fatal(err)
	}

	if got := announced(t, store); len(got) != 0 {
		t.Fatalf("retiring directories was announced: %v", got)
	}

	if got := entries(t, store); got != 2 {
		t.Fatalf("inventory has %d entries, want only the 2 files", got)
	}

	for path, tracked := range map[string]bool{
		root: false, filepath.Join(root, "site"): false, filepath.Join(root, untrackedIndex): true,
	} {
		_, exists, err := store.Current(t.Context(), []byte(path))
		if err != nil || exists != tracked {
			t.Fatalf("%s tracked=%v, want %v (%v)", path, exists, tracked, err)
		}
	}
}

//nolint:funlen // One ordered scenario: new tree, chmod, removal and a file replaced by a directory.
func TestUntrackedDirectoriesStillTrackFiles(t *testing.T) {
	t.Parallel()

	store, scanner, matcher, root := untrackedFixture(t)
	store.Policy.Directories = false

	_, err := store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "periodic", "")
	if err != nil {
		t.Fatal(err)
	}

	announced(t, store)

	plugins := filepath.Join(root, "site", "plugins")
	shell := filepath.Join(plugins, "evil", "shell.php")

	writeTree(t, root, "site/plugins/evil/shell.php")

	observe := func(path string) map[string]string {
		t.Helper()

		_, observeErr := store.Observe(t.Context(), scanner, []string{path}, []string{root}, matcher, "actor", "event")
		if observeErr != nil {
			t.Fatal(observeErr)
		}

		return announced(t, store)
	}

	if got, want := observe(plugins), map[string]string{shell: changeAdded}; !maps.Equal(got, want) {
		t.Fatalf("new tree announced %v, want %v", got, want)
	}

	err = os.Chmod(filepath.Dir(shell), 0o777) //nolint:gosec // The test checks that directory modes are not tracked.
	if err != nil {
		t.Fatal(err)
	}

	if got := observe(filepath.Dir(shell)); len(got) != 0 {
		t.Fatalf("directory chmod announced %v", got)
	}

	err = os.RemoveAll(plugins)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := observe(plugins), map[string]string{shell: changeDeleted}; !maps.Equal(got, want) {
		t.Fatalf("removed tree announced %v, want %v", got, want)
	}

	index := filepath.Join(root, untrackedIndex)

	err = os.Remove(index)
	if err == nil {
		err = os.Mkdir(index, 0o700)
	}

	if err != nil {
		t.Fatal(err)
	}

	if got, want := observe(index), map[string]string{index: changeDeleted}; !maps.Equal(got, want) {
		t.Fatalf("file replaced by a directory announced %v, want %v", got, want)
	}
}

// Tracking directories keeps the historical digest, so existing baselines stay valid.
func TestDirectoriesPolicyDigest(t *testing.T) {
	t.Parallel()

	historical := sha256.Sum256([]byte(fieldHash + "\x00" + fieldMode + "\x01sha256"))
	tracked := integrity.PolicyID([]string{fieldMode, fieldHash}, "sha256", true)

	if tracked != hex.EncodeToString(historical[:]) {
		t.Fatal("tracking directories changed the historical policy digest")
	}

	if integrity.PolicyID([]string{fieldMode, fieldHash}, "sha256", false) == tracked {
		t.Fatal("leaving directories out must change the policy digest")
	}
}

// A reference imported into a repository that does not track directories keeps only the files.
func TestImportSkipsUntrackedDirectories(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.Notifications.Use = nil
	store.Policy.Directories = false

	var input bytes.Buffer

	encoder := json.NewEncoder(&input)
	for _, record := range []integrity.Record{
		{Path: []byte("/srv/www"), Type: integrity.TypeDirectory},
		{Path: []byte("/srv/www/index.php"), Type: fixtureRegular, Algorithm: fixtureSHA256, Hash: strings.Repeat("a", 64)},
	} {
		err := encoder.Encode(record)
		if err != nil {
			t.Fatal(err)
		}
	}

	err := store.ImportBaseline(t.Context(), &input)
	if err != nil {
		t.Fatal(err)
	}

	if got := entries(t, store); got != 1 {
		t.Fatalf("imported %d entries, want only the file", got)
	}

	_, exists, err := store.Current(t.Context(), []byte("/srv/www"))
	if err != nil || exists {
		t.Fatal("directory imported into the reference", err)
	}
}

// A reference that only holds untracked directories imports nothing and must not become a baseline.
func TestImportOfOnlyUntrackedDirectoriesIsEmpty(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.Notifications.Use = nil
	store.Policy.Directories = false

	data, err := json.Marshal(integrity.Record{Path: []byte("/srv/www"), Type: integrity.TypeDirectory})
	if err != nil {
		t.Fatal(err)
	}

	err = store.ImportBaseline(t.Context(), bytes.NewReader(data))
	if err == nil {
		t.Fatal("reference without trackable records accepted")
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.BaselineReady || stats.Entries != 0 {
		t.Fatalf("empty import changed the catalog: %+v %v", stats, err)
	}
}
