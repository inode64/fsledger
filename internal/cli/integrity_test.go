package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

//nolint:funlen // Ordered integration fixture keeps setup, transitions and assertions together.
func TestDBCommandsAndExactApproval(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	source := filepath.Join(root, "source")

	err := os.Mkdir(source, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(source, "file")

	err = os.WriteFile(file, []byte("first"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	main := map[string]any{
		"runtime": filepath.Join(root, "run"),
		fixturePaths: map[string]any{
			"repositories": []string{fixtureInventoryYaml},
		},
		fixtureStorage: map[string]any{fixturePath: filepath.Join(root, fixtureStorage)},
	}

	repository := map[string]any{
		fixtureType:  "db",
		fixturePaths: []string{source},
		"integrity":  map[string]any{"compare": []string{"hash", fixtureType, "uid", "gid", "mode"}},
	}
	for name, document := range map[string]any{fixtureMainYaml: main, fixtureInventoryYaml: repository} {
		data, encodeErr := json.Marshal(document)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}

		err = os.WriteFile(filepath.Join(root, name), data, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	invoke := func(args ...string) ([]byte, error) {
		t.Helper()

		var output, stderr bytes.Buffer

		runErr := cli.Run(
			append(args, "-c", filepath.Join(root, fixtureMainYaml), fixtureRepoFlag, fixtureRepository),
			&output,
			&stderr,
		)

		return output.Bytes(), runErr
	}

	var initialOutput, initialError bytes.Buffer

	err = cli.Run(
		[]string{"baseline", "-c", filepath.Join(root, fixtureMainYaml), "init", fixtureRepoFlag, fixtureRepository},
		&initialOutput,
		&initialError,
	)

	output := initialOutput.Bytes()
	if err != nil {
		t.Fatalf("%s %v", output, err)
	}

	err = os.WriteFile(file, []byte("second"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	output, err = invoke("verify")
	if err == nil {
		t.Fatal("alteration was approved automatically")
	}

	var report struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}

	err = json.Unmarshal(output, &report)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(file, []byte("third"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	output, err = invoke("baseline", "accept", "--change", report.Result.ID)
	if err != nil {
		t.Fatalf("%s %v", output, err)
	}

	_, err = invoke("verify")
	if err == nil {
		t.Fatal("approval reread and accepted newer content")
	}

	verifyPolicyReplacement(t, root, source, report.Result.ID, invoke)

	_, err = os.Stat(filepath.Join(root, fixtureStorage, fixtureRepository, ".git"))
	if !os.IsNotExist(err) {
		t.Fatal("DB repository contains Git")
	}
}

func verifyPolicyReplacement(t *testing.T, root, source, oldChange string, invoke func(...string) ([]byte, error)) {
	t.Helper()

	document := fmt.Sprintf(`type: db
paths: [%s]
integrity:
  compare: [hash, type, uid, gid, mode]
  hash: {algorithm: blake3}
`, source)

	err := os.WriteFile(filepath.Join(root, fixtureInventoryYaml), []byte(document), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = invoke("verify")
	if err == nil {
		t.Fatal("new hash policy accepted old baseline")
	}

	output, err := invoke("baseline", "replace")
	if err != nil {
		t.Fatalf("%s %v", output, err)
	}

	_, err = invoke("verify")
	if err != nil {
		t.Fatal(err)
	}

	_, err = invoke("baseline", "accept", "--change", oldChange)
	if err == nil {
		t.Fatal("old policy observation approved")
	}
}
