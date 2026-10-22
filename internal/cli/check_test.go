package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

func TestCheckAcceptsAbsentSources(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	err = os.Mkdir(filepath.Join(root, "present"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	writeFiles(t, root, map[string]string{
		fixtureMainYaml: "runtime: " + root + "/run\nstorage: {path: " + root + "/state}\n" +
			"paths: {repositories: [system.yaml]}\n",
		fixtureSystemYaml: "paths: ['" + root + "/present', '" + root + "/absent', '" + root + "/keys/*.keyfile']\n",
	})

	var output, effective, stderr bytes.Buffer

	err = cli.Run([]string{checkCommand, "-c", filepath.Join(root, fixtureMainYaml)}, &output, &stderr)
	if err != nil || !strings.Contains(output.String(), "1 repositories; sources: 1 present, 2 absent, 0 skipped") {
		t.Fatalf("absent sources rejected or unreported: %v %q", err, output.String())
	}

	err = cli.Run(
		[]string{checkCommand, "--effective", "-c", filepath.Join(root, fixtureMainYaml)},
		&effective,
		&stderr,
	)
	if err != nil || !strings.Contains(effective.String(), "sources:") ||
		!strings.Contains(effective.String(), "- "+root+"/keys/*.keyfile") {
		t.Fatalf("effective configuration lacks resolved sources: %v %q", err, effective.String())
	}
}

func TestVerifyAndDoctorTolerateAbsentSources(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	err = os.Mkdir(filepath.Join(root, "present"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	run := inventoryRepository(t, root, "type: db\npaths: ['"+root+"/present', '"+root+"/absent/*.conf']\n")

	if changed := verifiedChanges(t, run); changed != 1 {
		t.Fatalf("verify recorded %d changes for the present source, want 1", changed)
	}

	var diagnosed, stderr bytes.Buffer

	err = cli.Run([]string{"doctor", "-c", filepath.Join(root, fixtureMainYaml)}, &diagnosed, &stderr)
	if err != nil || !strings.Contains(diagnosed.String(), root+"/absent/*.conf") ||
		strings.Contains(diagnosed.String(), `"path": "`+root+`/absent`) {
		t.Fatalf("doctor did not separate absent sources: %v %s", err, diagnosed.String())
	}
}

func TestVerifyRejectsIncompleteSourceSelection(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	present := filepath.Join(root, "present")

	err = os.Mkdir(present, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink("loop", filepath.Join(root, "loop"))
	if err != nil {
		t.Fatal(err)
	}

	run := inventoryRepository(t, root, "type: db\npaths: ['"+present+"', '"+root+"/loop/*']\n")

	output, err := run(verifyCommand)
	if err == nil || !strings.Contains(err.Error(), "incomplete source selection") {
		t.Fatalf("incomplete expansion was reconciled: %v %s", err, output)
	}
}
