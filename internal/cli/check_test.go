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

	for name, text := range map[string]string{
		fixtureMainYaml: "runtime: " + root + "/run\nstorage: {path: " + root + "/state}\n" +
			"paths: {repositories: [system.yaml]}\n",
		"system.yaml": "paths: ['" + root + "/present', '" + root + "/absent', '" + root + "/keys/*.keyfile']\n",
	} {
		err = os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

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

	for name, text := range map[string]string{
		fixtureMainYaml: "runtime: " + root + "/run\nstorage: {path: " + root + "/state}\n" +
			"paths: {repositories: [inventory.yaml]}\nintegrity: {reference: previous}\n",
		"inventory.yaml": "type: db\npaths: ['" + root + "/present', '" + root + "/absent/*.conf']\n",
	} {
		err = os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	var verified, diagnosed, stderr bytes.Buffer

	err = cli.Run(
		[]string{verifyCommand, "-c", filepath.Join(root, fixtureMainYaml), fixtureRepoFlag, fixtureRepository},
		&verified,
		&stderr,
	)
	if err != nil {
		t.Fatalf("verify rejected an absent source: %v %s", err, verified.String())
	}

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

	for name, text := range map[string]string{
		fixtureMainYaml: "runtime: " + root + "/run\nstorage: {path: " + root + "/state}\n" +
			"paths: {repositories: [inventory.yaml]}\n",
		"inventory.yaml": "type: db\npaths: ['" + present + "', '" + root + "/loop/*']\n",
	} {
		err = os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	var output, stderr bytes.Buffer

	err = cli.Run(
		[]string{verifyCommand, "-c", filepath.Join(root, fixtureMainYaml), fixtureRepoFlag, fixtureRepository},
		&output,
		&stderr,
	)
	if err == nil || !strings.Contains(err.Error(), "incomplete source selection") {
		t.Fatalf("incomplete expansion was reconciled: %v %s", err, output.String())
	}
}
