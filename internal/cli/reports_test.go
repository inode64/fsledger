package cli_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

func TestReportPreviewUsesCatalogWithoutDelivery(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"source":    "private source contents",
		"mail.yaml": fixtureMailNotifier,
		"repo.yaml": "type: db\npaths: ['" + root + "/source']\n",
		fixtureMainYaml: "runtime: '" + root + "/run'\nstorage: {path: '" + root + "/state'}\n" +
			"paths: {repositories: [repo.yaml], notifiers: [mail.yaml]}\n" +
			"reports: {enabled: true, use: [mail]}\n",
	})

	var output, stderr bytes.Buffer

	arguments := []string{"-c", filepath.Join(root, fixtureMainYaml), "--repository", "repo"}

	err := cli.Run(append([]string{verifyCommand}, arguments...), &output, &stderr)
	if !errors.Is(err, cli.ErrChanges) {
		t.Fatal(output.String(), err)
	}

	for range 2 {
		output.Reset()

		err = cli.Run(append([]string{"report", "preview"}, arguments...), &output, &stderr)
		if err != nil || !strings.Contains(output.String(), root+"/source") ||
			strings.Contains(output.String(), "private source contents") {
			t.Fatal("preview lost metadata or exposed file content", output.String(), err)
		}
	}
}
