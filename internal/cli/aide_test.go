package cli_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	fixturePaths         = "paths"
	fixtureMainYaml      = "main.yaml"
	checkCommand         = "check"
	fixtureInventoryYaml = "inventory.yaml"
	fixtureStorage       = "storage"
	fixturePath          = "path"
	fixtureType          = "type"
	fixtureRepository    = "inventory"
	fixtureRepoFlag      = "--repository"
)

type aideFixture struct {
	configuration string
	source        string
	database      string
	catalog       string
	root          string
}

func writeAIDEFixture(t *testing.T, kind string, gzipInput bool) aideFixture {
	t.Helper()
	root := t.TempDir()

	source := filepath.Join(root, "data")

	err := os.Mkdir(source, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"changed", "deleted", "untouched"} {
		err = os.WriteFile(filepath.Join(source, name), []byte(name), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	err = os.Symlink("0 target", filepath.Join(source, "link"))
	if err != nil {
		t.Fatal(err)
	}

	data := aideFixtureData(t, source)
	database := filepath.Join(root, "old.db")

	if gzipInput {
		data = compressAIDEFixture(t, data)
	}

	err = os.WriteFile(database, data, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	writeAIDEConfiguration(t, kind, root, source)

	configuration := filepath.Join(root, fixtureMainYaml)

	cfg, err := config.Load(configuration)
	if err != nil {
		t.Fatal(err)
	}

	return aideFixture{
		configuration: configuration,
		source:        source,
		database:      database,
		catalog:       cfg.CatalogPath("inventory"),
		root:          root,
	}
}

func aideFixtureData(t *testing.T, source string) []byte {
	t.Helper()

	data := []byte("@@begin_db\n@@db_spec name perm inode lname sha256\n")

	for _, name := range []string{"", "changed", "deleted", "untouched", "link"} {
		path := filepath.Join(source, name)

		record, err := integrity.NewScanner(1, 2).Observe(t.Context(), path, "sha256", true)
		if err != nil {
			t.Fatal(err)
		}

		digest, target := "0", "0"

		if record.Type == "regular" {
			sum := sha256.Sum256([]byte(name))
			digest = base64.StdEncoding.EncodeToString(sum[:])
		}

		if record.Type == "symlink" {
			target = "00%20target"
		}

		data = fmt.Appendf(data, "%s %o %d %s %s\n", path, record.Mode, record.Inode, target, digest)
	}

	data = fmt.Appendf(data, "%s/.git/config 100600 1 0 0\n%s/ignored/item 100600 1 0 0\n", source, source)

	// A single system-wide AIDE database can contain paths outside this repository.
	data = append(data, []byte("/unselected 100600 1 0 0\n@@end_db\n")...)

	return data
}

func compressAIDEFixture(t *testing.T, data []byte) []byte {
	t.Helper()

	var compressed bytes.Buffer

	writer := gzip.NewWriter(&compressed)

	_, err := writer.Write(data)
	if err != nil {
		t.Fatal(err)
	}

	err = writer.Close()
	if err != nil {
		t.Fatal(err)
	}

	data = compressed.Bytes()

	return data
}

func writeAIDEConfiguration(t *testing.T, kind, root, source string) {
	t.Helper()

	main := map[string]any{
		"runtime": filepath.Join(root, "run"),
		fixturePaths: map[string]any{
			"repositories": []string{fixtureInventoryYaml},
			"notifiers":    []string{"sink.yaml"},
			"excludes":     []string{"skip.yaml"},
		},
		fixtureStorage: map[string]any{fixturePath: filepath.Join(root, "default")},
	}

	repository := map[string]any{
		fixtureType:     kind,
		fixturePaths:    []string{source},
		fixtureStorage:  map[string]any{fixturePath: filepath.Join(root, "custom")},
		"integrity":     map[string]any{"compare": []string{"hash", fixtureType, "inode", "target"}},
		"notifications": map[string]any{"use": []string{"sink"}},
		"exclude":       []string{"skip"},
	}
	for name, document := range map[string]any{
		fixtureMainYaml: main, fixtureInventoryYaml: repository,
		"skip.yaml": []string{"**/ignored/**"},
		"sink.yaml": map[string]any{fixtureType: "slack", "url": "https://example.invalid/not-contacted"},
	} {
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(filepath.Join(root, name), encoded, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func runMigrationCLI(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()

	var output, stderr bytes.Buffer

	err := cli.Run(args, &output, &stderr)

	return output.Bytes(), err
}

//nolint:funlen,gocognit // Import, refusal and verification form one ordered real-catalog lifecycle.
func TestMigrateAIDEKeepsOriginalEvidence(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{config.RepositoryDB, config.RepositoryGit} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			fixture := writeAIDEFixture(t, kind, kind == config.RepositoryDB)

			err := os.WriteFile(
				filepath.Join(fixture.source, "changed"),
				[]byte("changed before import"),
				0o600,
			)
			if err != nil {
				t.Fatal(err)
			}

			err = os.Remove(filepath.Join(fixture.source, "deleted"))
			if err != nil {
				t.Fatal(err)
			}

			output, err := runMigrationCLI(
				t,
				"migrate_aide",
				fixture.database,
				"inventory",
				"-c",
				fixture.configuration,
			)
			if err != nil {
				t.Fatalf("import: %s %v", output, err)
			}

			var report struct {
				Catalog  string `json:"catalog"`
				Imported int    `json:"imported"`
				Skipped  int    `json:"skipped"`
			}

			err = json.Unmarshal(
				output,
				&report,
			)
			if err != nil || report.Imported != 5 || report.Skipped != 3 ||
				report.Catalog != fixture.catalog {
				t.Fatalf("wrong import result: %s %v", output, err)
			}

			_, err = runMigrationCLI(t, "migrate_aide", "-c", fixture.configuration, fixture.database, "inventory")
			if err == nil || !strings.Contains(err.Error(), "empty catalog") {
				t.Fatal("existing baseline overwritten", err)
			}

			output, err = runMigrationCLI(t, "verify", "-c", fixture.configuration, "--repository", "inventory")
			if err == nil {
				t.Fatal("migration approved modified source content")
			}

			var verified struct {
				Status struct {
					Violations int `json:"violations"`
				} `json:"status"`
			}

			err = json.Unmarshal(output, &verified)
			if err != nil || verified.Status.Violations != 2 {
				t.Fatalf("lost original changes: %s %v", output, err)
			}
		})
	}
}

func TestMigrateAIDERejectsTruncatedInputBeforeOpeningDestination(t *testing.T) {
	t.Parallel()

	fixture := writeAIDEFixture(t, config.RepositoryDB, false)

	data, err := os.ReadFile(fixture.database)
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // The destination is the private database created by this test fixture.
	err = os.WriteFile(fixture.database, bytes.TrimSuffix(data, []byte("@@end_db\n")), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = runMigrationCLI(t, "migrate_aide", fixture.database, "inventory", "-c", fixture.configuration)
	if err == nil {
		t.Fatal("incomplete input approved")
	}

	_, err = os.Stat(filepath.Join(fixture.root, "custom"))
	if !os.IsNotExist(err) {
		t.Fatal("invalid input touched destination", err)
	}
}

func TestMigrateAIDERespectsRepositoryLock(t *testing.T) {
	t.Parallel()
	fixture := writeAIDEFixture(t, config.RepositoryDB, false)

	lock, err := resource.LockDirectory(filepath.Join(fixture.root, "custom", "inventory"))
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(lock)

	_, err = runMigrationCLI(t, "migrate_aide", fixture.database, "inventory", "-c", fixture.configuration)
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatal("import bypassed repository lock", err)
	}
}

func TestMigrateAIDEWithMissingSources(t *testing.T) {
	t.Parallel()
	fixture := writeAIDEFixture(t, config.RepositoryDB, false)

	err := os.Rename(fixture.source, fixture.source+".offline")
	if err != nil {
		t.Fatal(err)
	}

	output, err := runMigrationCLI(t, "migrate_aide", fixture.database, "inventory", "-c", fixture.configuration)
	if err != nil {
		t.Fatalf("offline import: %s %v", output, err)
	}
}
