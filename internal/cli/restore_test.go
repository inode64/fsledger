package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/cli"
)

const (
	restoreCommand  = "restore"
	restoreRepoName = "repo"
	restoreRepoYaml = "repo.yaml"
	dateFlag        = "--date"
	motdPath        = "/etc/motd"
	firstVersion    = "v1\n"
	secondVersion   = "v2\n"
)

// restoreFixture prepares a configuration whose git repository mirror already holds etc/motd
// as v1 (committed 2026-03-01) and v2 (committed 2026-03-03).
func restoreFixture(t *testing.T) (string, func(args ...string) (string, error)) {
	t.Helper()

	root := t.TempDir()
	mirror := filepath.Join(root, "state", restoreRepoName)

	writeFiles(t, root, map[string]string{
		restoreRepoYaml: "type: git\npaths: ['" + root + "/source']\n",
		fixtureMainYaml: "runtime: '" + root + "/run'\nstorage: {path: '" + root + "/state'}\n" +
			"paths: {repositories: [" + restoreRepoYaml + ", db.yaml]}\n",
		"db.yaml": "type: db\npaths: ['" + root + "/source']\n",
	})

	for _, directory := range []string{filepath.Join(root, "source"), filepath.Join(mirror, "etc")} {
		err := os.MkdirAll(directory, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	gitRun(t, mirror, time.Time{}, "init", "-q")
	writeMotd(t, mirror, firstVersion)
	gitRun(t, mirror, time.Date(2026, time.March, 1, 10, 0, 0, 0, time.UTC), "add", "-A")
	gitRun(t, mirror, time.Date(2026, time.March, 1, 10, 0, 0, 0, time.UTC), "commit", "-q", "-m", "first")
	writeMotd(t, mirror, secondVersion)
	gitRun(t, mirror, time.Date(2026, time.March, 3, 10, 0, 0, 0, time.UTC), "add", "-A")
	gitRun(t, mirror, time.Date(2026, time.March, 3, 10, 0, 0, 0, time.UTC), "commit", "-q", "-m", "second")

	invoke := func(args ...string) (string, error) {
		var output, stderr bytes.Buffer

		err := cli.Run(append([]string{restoreCommand, "-c", filepath.Join(root, fixtureMainYaml)}, args...),
			&output, &stderr)

		return output.String(), err
	}

	return root, invoke
}

func writeMotd(t *testing.T, mirror, body string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(mirror, "etc", "motd"), []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func gitRun(t *testing.T, mirror string, when time.Time, args ...string) {
	t.Helper()

	//nolint:gosec // Explicit Git arguments and a private temporary repository, without a shell.
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", mirror}, args...)...)
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org",
		"GIT_AUTHOR_DATE="+when.Format(time.RFC3339), "GIT_COMMITTER_DATE="+when.Format(time.RFC3339),
	)

	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal(string(output), err)
	}
}

func TestRestoreWritesVersionAtDate(t *testing.T) {
	t.Parallel()

	root, invoke := restoreFixture(t)
	target := filepath.Join(root, "out", "motd")

	err := os.Mkdir(filepath.Join(root, "out"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	output, err := invoke(fixtureRepoFlag, restoreRepoName, dateFlag, "2026-03-02T00:00:00Z", motdPath, target)
	if err != nil || !strings.Contains(output, "restored "+target+"\n") ||
		!strings.Contains(output, "Restored 1, skipped 0, failed 0 from ") ||
		!strings.Contains(output, "Ownership, ACLs and extended attributes") {
		t.Fatal(output, err)
	}

	content, err := os.ReadFile(target) //nolint:gosec // Test reads its own temporary destination.
	if err != nil || string(content) != firstVersion {
		t.Fatal(string(content), err)
	}

	output, err = invoke(fixtureRepoFlag, restoreRepoName, dateFlag, "2026-03-04T00:00:00Z", "etc/motd", target)
	if err != nil || !strings.Contains(output, "skipped "+target+": exists") ||
		!strings.Contains(output, "Restored 0, skipped 1, failed 0") {
		t.Fatal(output, err)
	}

	output, err = invoke(fixtureRepoFlag, restoreRepoName, dateFlag, "2026-03-04T00:00:00Z", "--overwrite", "always",
		"etc/motd", target)
	if err != nil || !strings.Contains(output, "Restored 1, skipped 0, failed 0") {
		t.Fatal(output, err)
	}

	content, err = os.ReadFile(target) //nolint:gosec // Test reads its own temporary destination.
	if err != nil || string(content) != secondVersion {
		t.Fatal(string(content), err)
	}
}

func TestRestoreDirectoryIntoDestination(t *testing.T) {
	t.Parallel()

	root, invoke := restoreFixture(t)
	target := filepath.Join(root, "out", "etc")

	output, err := invoke(fixtureRepoFlag, restoreRepoName, dateFlag, "2026-03-03T23:00:00Z", "/etc", target)
	if err != nil || !strings.Contains(output, "restored "+filepath.Join(target, "motd")+"\n") {
		t.Fatal(output, err)
	}

	content, err := os.ReadFile(filepath.Join(target, "motd")) //nolint:gosec // Test reads its own destination.
	if err != nil || string(content) != secondVersion {
		t.Fatal(string(content), err)
	}
}

func TestRestoreDryRunCreatesNothing(t *testing.T) {
	t.Parallel()

	root, invoke := restoreFixture(t)
	target := filepath.Join(root, "out", "etc")

	output, err := invoke(fixtureRepoFlag, restoreRepoName, dateFlag, "2026-03-03", "--dry-run", "/etc", target)
	if err != nil || !strings.Contains(output, "restored "+filepath.Join(target, "motd")+"\n") {
		t.Fatal(output, err)
	}

	_, err = os.Lstat(filepath.Join(root, "out"))
	if err == nil {
		t.Fatal("dry run created the destination")
	}
}

func TestRestoreRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	root, invoke := restoreFixture(t)
	target := filepath.Join(root, "out", "motd")
	valid := []string{fixtureRepoFlag, restoreRepoName, dateFlag, "2026-03-02"}

	for name, scenario := range map[string]struct {
		want string
		args []string
	}{
		"db repository": {want: "git", args: []string{fixtureRepoFlag, "db", dateFlag, "2026-03-02", motdPath, target}},
		"invalid date": {
			want: dateFlag, args: []string{fixtureRepoFlag, restoreRepoName, dateFlag, "yesterday", motdPath},
		},
		"unknown policy":  {want: "--overwrite", args: append(valid, "--overwrite", "maybe", motdPath, target)},
		"missing operand": {want: "usage", args: valid},
		"too early": {
			want: "2026-02-01", args: []string{fixtureRepoFlag, restoreRepoName, dateFlag, "2026-02-01", motdPath, target},
		},
		"missing path":    {want: "etc/absent", args: append(valid, "/etc/absent", target)},
		"relative target": {want: "absolute", args: append(valid, motdPath, "out/motd")},
		"git component":   {want: ".git", args: append(valid, "/.git/config", target)},
	} {
		output, err := invoke(scenario.args...)
		if err == nil || !strings.Contains(err.Error(), scenario.want) {
			t.Fatalf("%s: %q %v", name, output, err)
		}
	}

	_, err := os.Lstat(filepath.Join(root, "out"))
	if err == nil {
		t.Fatal("a rejected request created the destination")
	}
}
