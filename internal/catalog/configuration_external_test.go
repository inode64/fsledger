package catalog_test

import (
	"os"
	"path/filepath"
	"testing"
)

func testConfiguration(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	for name, data := range map[string]string{
		"main.yaml": "paths: {repositories: [repo.yaml]}\n",
		"repo.yaml": "paths: [/tmp/fsledger-test-source]\n",
	} {
		err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	return filepath.Join(root, "main.yaml")
}
