package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

const baselineCommand = "baseline"

// pointLink replaces the source parent link, so a test can make that source unreadable and back.
func pointLink(t *testing.T, link, target string) {
	t.Helper()

	err := os.Remove(link)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	err = os.Symlink(target, link)
	if err != nil {
		t.Fatal(err)
	}
}

func TestBaselineReplaceRefusesUnreadableLiteralSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	link := filepath.Join(root, "link")

	err := os.MkdirAll(filepath.Join(root, "real", "data"), 0o700)
	if err == nil {
		err = os.WriteFile(filepath.Join(root, "real", "data", "file"), []byte("approved"), 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}

	pointLink(t, link, "real")

	for name, text := range map[string]string{
		fixtureMainYaml: "runtime: " + root + "/run\nstorage: {path: " + root + "/state}\n" +
			"paths: {repositories: [" + fixtureInventoryYaml + "]}\n",
		fixtureInventoryYaml: "type: db\npaths: ['" + link + "/data']\n",
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

	output, err := invoke(baselineCommand, "init")
	if err != nil {
		t.Fatalf("%s %v", output, err)
	}

	// A symlink loop exceeds the hop limit, which leaves the source unread even for root.
	pointLink(t, link, "link")

	output, err = invoke(baselineCommand, "replace")
	if err == nil || !strings.Contains(err.Error(), "incomplete source selection") ||
		!strings.Contains(err.Error(), link+"/data") {
		t.Fatalf("unreadable source replaced the baseline: %v %s", err, output)
	}

	pointLink(t, link, "real")

	output, err = invoke(verifyCommand)
	if err != nil {
		t.Fatalf("approved baseline lost the unreadable source: %v %s", err, output)
	}
}
