package cli_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

const changesCommand = "changes"

// writeInventoryFixture configures one repository of the given type whose storage does not exist yet.
func writeInventoryFixture(t *testing.T, kind string) (string, func(...string) (string, error)) {
	t.Helper()

	root := t.TempDir()

	err := os.Mkdir(filepath.Join(root, "source"), 0o700)
	if err == nil {
		err = os.WriteFile(filepath.Join(root, "source", "file"), []byte("approved"), 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}

	for name, text := range map[string]string{
		fixtureMainYaml: "runtime: " + root + "/run\nstorage: {path: " + root + "/state}\n" +
			"paths: {repositories: [" + fixtureInventoryYaml + "]}\n",
		fixtureInventoryYaml: "type: " + kind + "\npaths: ['" + root + "/source']\n",
	} {
		err = os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	invoke := func(args ...string) (string, error) {
		t.Helper()

		var output, stderr bytes.Buffer

		runErr := cli.Run(
			append(args, "-c", filepath.Join(root, fixtureMainYaml), fixtureRepoFlag, fixtureRepository),
			&output,
			&stderr,
		)

		return output.String(), runErr
	}

	return root, invoke
}

func TestInventoryUsageErrorsCreateNoState(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"db", "git"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			root, invoke := writeInventoryFixture(t, kind)

			for name, args := range map[string][]string{
				"missing action":          {baselineCommand},
				"misspelled action":       {baselineCommand, "inti"},
				"accept without change":   {baselineCommand, "accept"},
				"import without input":    {baselineCommand, "import"},
				"changes without catalog": {changesCommand},
			} {
				output, err := invoke(args...)
				if err == nil {
					t.Fatalf("%s accepted: %s", name, output)
				}

				_, err = os.Lstat(filepath.Join(root, "state"))
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s created repository state: %v", name, err)
				}
			}
		})
	}
}

func TestChangesListsExistingCatalogOnly(t *testing.T) {
	t.Parallel()

	root, invoke := writeInventoryFixture(t, "git")

	_, err := invoke(changesCommand)
	if err == nil || !strings.Contains(err.Error(), "repository has no catalog") {
		t.Fatalf("changes without catalog: %v", err)
	}

	output, err := invoke(baselineCommand, "init")
	if err != nil {
		t.Fatalf("%s %v", output, err)
	}

	err = os.WriteFile(filepath.Join(root, "source", "file"), []byte("altered"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = invoke(verifyCommand)
	if err == nil {
		t.Fatal("alteration was approved automatically")
	}

	output, err = invoke(changesCommand)
	if err != nil || !strings.Contains(output, filepath.Join(root, "source", "file")) {
		t.Fatalf("changes missed the recorded difference: %v %s", err, output)
	}
}
