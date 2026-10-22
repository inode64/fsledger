package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

const (
	fixturePaths         = "paths"
	fixtureMainYaml      = "main.yaml"
	checkCommand         = "check"
	fixtureInventoryYaml = "inventory.yaml"
	fixturePath          = "path"
	fixtureType          = "type"
	fixtureRepository    = "inventory"
	fixtureRepoFlag      = "--repository"
	fixtureSystemYaml    = "system.yaml"
	fixtureState         = "state"
	verifyCommand        = "verify"
	// fixtureMailNotifier is an email notifier whose relay never resolves, so nothing is sent.
	fixtureMailNotifier = "type: email\ndsn: smtps://unreachable.invalid\nfrom: sender@example.org\n" +
		"to: [reader@example.org]\n"
)

// writeFiles writes a test configuration tree under root, creating parent directories as needed.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for name, text := range files {
		path := filepath.Join(root, name)

		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err == nil {
			err = os.WriteFile(path, []byte(text), 0o600)
		}

		if err != nil {
			t.Fatal(err)
		}
	}
}

// inventoryRepository configures under root a single repository named inventory, described by
// repository, with its state in root/state, and returns a runner of CLI commands for it.
func inventoryRepository(t *testing.T, root, repository string) func(...string) (string, error) {
	t.Helper()

	writeFiles(t, root, map[string]string{
		fixtureMainYaml: "runtime: " + root + "/run\nstorage: {path: " + root + "/" + fixtureState + "}\n" +
			"paths: {repositories: [" + fixtureInventoryYaml + "]}\n",
		fixtureInventoryYaml: repository,
	})

	return func(args ...string) (string, error) {
		t.Helper()

		var output, stderr bytes.Buffer

		err := cli.Run(
			append(args, "-c", filepath.Join(root, fixtureMainYaml), fixtureRepoFlag, fixtureRepository),
			&output,
			&stderr,
		)

		return output.String(), err
	}
}

// inventorySource configures under root an inventory repository of root/source, which holds one file,
// with integrity settings integrity, verifies it once and returns the file and the repository runner.
func inventorySource(t *testing.T, root, integrity string) (string, func(...string) (string, error)) {
	t.Helper()

	writeFiles(t, root, map[string]string{"source/file": "first"})

	source := filepath.Join(root, "source")
	run := inventoryRepository(t, root, "type: db\npaths: ['"+source+"']\nintegrity: "+integrity+"\n")

	if changed := verifiedChanges(t, run); changed == 0 {
		t.Fatal("first verification recorded no observation")
	}

	return filepath.Join(source, "file"), run
}

// verifiedChanges runs verify and returns how many changes it recorded; only a verification that
// recorded changes may exit with status 2.
func verifiedChanges(t *testing.T, run func(...string) (string, error)) int {
	t.Helper()

	output, err := run(verifyCommand)
	if err != nil && !errors.Is(err, cli.ErrChanges) {
		t.Fatalf("%s %v", output, err)
	}

	var report struct {
		Result struct {
			Changed int `json:"changed"`
		} `json:"result"`
	}

	decodeErr := json.Unmarshal([]byte(output), &report)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}

	if errors.Is(err, cli.ErrChanges) != (report.Result.Changed > 0) {
		t.Fatalf("exit error %v disagrees with %d recorded changes", err, report.Result.Changed)
	}

	return report.Result.Changed
}
