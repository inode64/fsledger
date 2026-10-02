package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

const verifyCommand = "verify"

func TestReportPreviewUsesCatalogWithoutDelivery(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for name, body := range map[string]string{
		"source": "private source contents",
		"mail.yaml": `type: email
dsn: smtps://unreachable.invalid
from: sender@example.org
to: [reader@example.org]
`,
		"repo.yaml": "type: db\npaths: ['" + root + "/source']\n",
		fixtureMainYaml: "runtime: '" + root + "/run'\nstorage: {path: '" + root + "/state'}\n" +
			"paths: {repositories: [repo.yaml], notifiers: [mail.yaml]}\nintegrity: {reference: previous}\n" +
			"reports: {enabled: true, use: [mail]}\n",
	} {
		err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	var output, stderr bytes.Buffer

	arguments := []string{"-c", filepath.Join(root, fixtureMainYaml), "--repository", "repo"}

	err := cli.Run(append([]string{verifyCommand}, arguments...), &output, &stderr)
	if err != nil {
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
