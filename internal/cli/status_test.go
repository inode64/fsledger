package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/cli"
	"github.com/inode64/fsledger/internal/daemon"
)

// writeStatusFixture configures web and zz but publishes a snapshot only for zz.
func writeStatusFixture(t *testing.T, root string) {
	t.Helper()

	runtime := filepath.Join(root, "run")

	err := os.Mkdir(runtime, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := json.Marshal(daemon.Status{
		Repository: "zz", Type: "db", LastCommit: "published", Updated: time.Now(), Running: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string][]byte{
		filepath.Join("run", "zz.json"): snapshot,
		"web.yaml":                      []byte("type: db\npaths: ['" + root + "/web']\n"),
		"zz.yaml":                       []byte("type: db\npaths: ['" + root + "/zz']\n"),
		fixtureMainYaml: []byte("runtime: '" + runtime + "'\nstorage: {path: '" + root + "/state'}\n" +
			"paths: {repositories: [web.yaml, zz.yaml]}\n"),
	} {
		err = os.WriteFile(filepath.Join(root, name), body, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// decodeStatuses parses the status output as the stream of JSON objects it promises.
func decodeStatuses(t *testing.T, output string) []daemon.Status {
	t.Helper()

	var states []daemon.Status

	decoder := json.NewDecoder(strings.NewReader(output))

	for {
		var state daemon.Status

		err := decoder.Decode(&state)
		if errors.Is(err, io.EOF) {
			return states
		}

		if err != nil {
			t.Fatalf("status output is not a JSON object stream: %v; output %q", err, output)
		}

		states = append(states, state)
	}
}

// A repository the daemon has not published yet must not hide the snapshots of the others.
func TestStatusReportsMissingSnapshotAndContinues(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeStatusFixture(t, root)

	var stdout, stderr bytes.Buffer

	err := cli.Run([]string{"status", "-c", filepath.Join(root, fixtureMainYaml)}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("status error = %v; stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}

	states := decodeStatuses(t, stdout.String())
	if len(states) != 2 {
		t.Fatalf("status printed %d objects, want one per repository: %q", len(states), stdout.String())
	}

	missing, present := states[0], states[1]
	if missing.Repository != "web" || missing.Type != "db" || missing.Running ||
		!slices.ContainsFunc(missing.Warnings, func(warning string) bool {
			return strings.HasPrefix(warning, "no status snapshot")
		}) {
		t.Fatalf("missing snapshot reported as %+v; want web not running with a warning", missing)
	}

	if present.Repository != "zz" || present.LastCommit != "published" {
		t.Fatalf("present snapshot reported as %+v; want the zz snapshot", present)
	}
}
