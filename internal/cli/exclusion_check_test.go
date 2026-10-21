package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

// TestCheckNormalizesTrailingSlashAndRejectsDotExclusions keeps deployed gitignore-style
// patterns valid while reporting the ones that can never match.
func TestCheckNormalizesTrailingSlashAndRejectsDotExclusions(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for name, text := range map[string]string{
		fixtureMainYaml: "runtime: " + root + "/run\nstorage: {path: " + root + "/state}\n" +
			"paths: {repositories: [system.yaml], excludes: [excludes]}\nexclude: [common]\n",
		fixtureSystemYaml:      "paths: ['" + root + "/src']\n",
		"excludes/common.yaml": "- " + root + "/src/private/\n- '**/node_modules/'\n",
	} {
		err = os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	var output, stderr bytes.Buffer

	mainPath := filepath.Join(root, fixtureMainYaml)

	err = cli.Run([]string{checkCommand, "-c", mainPath}, &output, &stderr)
	if err != nil {
		t.Fatalf("trailing slash rejected: %v", err)
	}

	err = os.WriteFile(filepath.Join(root, "excludes", "common.yaml"), []byte("- "+root+"/src/../src/private\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = cli.Run([]string{checkCommand, "-c", mainPath}, &output, &stderr)
	if err == nil || !strings.Contains(err.Error(), `invalid exclusion "`+root+`/src/../src/private"`) {
		t.Fatalf("dot component accepted or unexplained: %v", err)
	}
}
