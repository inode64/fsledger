package restore_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/restore"
)

const (
	prefix     = "etc/ssh"
	configName = "sshd_config"
	// globalHeader is the name git archive gives its pax global header.
	globalHeader = "pax_global_header"
	configBody   = "archived config\n"
)

type entry struct {
	name     string
	body     string
	linkname string
	typeflag byte
	mode     int64
}

var committed = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC) //nolint:gochecknoglobals // Fixed fixture date.

func file(name, body string) entry {
	return entry{name: name, typeflag: tar.TypeReg, mode: 0o644, body: body, linkname: ""}
}

func executable(name, body string) entry {
	return entry{name: name, typeflag: tar.TypeReg, mode: 0o755, body: body, linkname: ""}
}

func directory(name string) entry {
	return entry{name: name + "/", typeflag: tar.TypeDir, mode: 0o755, body: "", linkname: ""}
}

func symlink(name, target string) entry {
	return entry{name: name, typeflag: tar.TypeSymlink, mode: 0o777, body: "", linkname: target}
}

func fifo(name string) entry {
	return entry{name: name, typeflag: tar.TypeFifo, mode: 0o644, body: "", linkname: ""}
}

// buildTar mimics git archive: a global header, every ancestor directory, then the entries.
func buildTar(t *testing.T, entries ...entry) *tar.Reader {
	t.Helper()

	var buffer bytes.Buffer

	writer := tar.NewWriter(&buffer)

	//nolint:exhaustruct_v5 // Git only sets its name and the commit comment on its global header.
	err := writer.WriteHeader(&tar.Header{
		Name: globalHeader, Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "deadbeef"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, item := range append([]entry{directory("etc"), directory(prefix)}, entries...) {
		//nolint:exhaustruct_v5 // Git archives carry no ownership, device or access times worth mimicking.
		header := &tar.Header{
			Name: item.name, Typeflag: item.typeflag, Mode: item.mode, Linkname: item.linkname,
			Size: int64(len(item.body)), ModTime: committed,
		}

		err = writer.WriteHeader(header)
		if err != nil {
			t.Fatal(err)
		}

		_, err = writer.Write([]byte(item.body))
		if err != nil {
			t.Fatal(err)
		}
	}

	err = writer.Close()
	if err != nil {
		t.Fatal(err)
	}

	return tar.NewReader(&buffer)
}

type run struct {
	destination string
	output      bytes.Buffer
	summary     restore.Summary
}

func options(policy restore.Policy, destination string, output io.Writer) restore.Options {
	return restore.Options{
		Committed: committed, Output: output, Destination: destination, Prefix: prefix, Policy: policy, DryRun: false,
	}
}

func apply(t *testing.T, policy restore.Policy, destination string, entries ...entry) run {
	t.Helper()

	result := run{destination: destination, output: bytes.Buffer{}, summary: restore.Summary{}}

	summary, err := restore.Apply(t.Context(), buildTar(t, entries...), options(policy, destination, &result.output))
	if err != nil {
		t.Fatal(err)
	}

	result.summary = summary

	return result
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path) //nolint:gosec // Test reads its own temporary destination.
	if err != nil {
		t.Fatal(err)
	}

	return string(content)
}

func writeFile(t *testing.T, path, body string, mode fs.FileMode, mtime time.Time) {
	t.Helper()

	err := os.WriteFile(path, []byte(body), mode)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Chtimes(path, mtime, mtime)
	if err != nil {
		t.Fatal(err)
	}
}

func TestApplyRestoresTreeUnderPrefix(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()
	result := apply(t, restore.Off, destination,
		file(prefix+"/"+configName, configBody),
		executable(prefix+"/helper", "#!/bin/sh\n"),
		symlink(prefix+"/link", configName),
		directory(prefix+"/sub"),
		file(prefix+"/sub/inner", "inner"),
	)

	if result.summary != (restore.Summary{Restored: 5, Skipped: 0, Failed: 0}) {
		t.Fatal(result.summary, result.output.String())
	}

	if readFile(t, filepath.Join(destination, configName)) != configBody {
		t.Fatal("config content")
	}

	for name, want := range map[string]fs.FileMode{configName: 0o644, "helper": 0o755, "sub": fs.ModeDir | 0o755} {
		info, err := os.Lstat(filepath.Join(destination, name))
		if err != nil || info.Mode() != want {
			t.Fatalf("%s: %v %v", name, info.Mode(), err)
		}
	}

	target, err := os.Readlink(filepath.Join(destination, "link"))
	if err != nil || target != configName {
		t.Fatal(target, err)
	}

	if strings.Count(result.output.String(), "restored "+destination+"/") != 5 {
		t.Fatal(result.output.String())
	}

	_, err = os.Lstat(filepath.Join(destination, "etc"))
	if err == nil {
		t.Fatal("ancestor directory was recreated inside the destination")
	}
}

func TestApplyOffKeepsExistingFiles(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()
	writeFile(t, filepath.Join(destination, configName), "local", 0o600, committed.Add(-time.Hour))

	result := apply(t, restore.Off, destination, file(prefix+"/"+configName, configBody))
	if result.summary != (restore.Summary{Restored: 0, Skipped: 1, Failed: 0}) ||
		!strings.Contains(result.output.String(), "skipped "+destination+"/"+configName+": exists") {
		t.Fatal(result.summary, result.output.String())
	}

	if readFile(t, filepath.Join(destination, configName)) != "local" {
		t.Fatal("existing file was replaced")
	}
}

func TestApplyAlwaysReplacesKeepingModeAndOwner(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()
	writeFile(t, filepath.Join(destination, configName), "local", 0o600, committed.Add(time.Hour))

	result := apply(t, restore.Always, destination, file(prefix+"/"+configName, configBody))
	if result.summary != (restore.Summary{Restored: 1, Skipped: 0, Failed: 0}) {
		t.Fatal(result.summary, result.output.String())
	}

	info, err := os.Lstat(filepath.Join(destination, configName))
	if err != nil || info.Mode() != 0o600 {
		t.Fatal(info.Mode(), err)
	}

	if readFile(t, filepath.Join(destination, configName)) != configBody {
		t.Fatal("content was not replaced")
	}

	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary file left behind", entries, err)
	}
}

func TestApplyNewerComparesCommitDateWithDestination(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()
	writeFile(t, filepath.Join(destination, "older"), "local", 0o644, committed.Add(-time.Hour))
	writeFile(t, filepath.Join(destination, "newer"), "local", 0o644, committed.Add(time.Hour))

	result := apply(t, restore.Newer, destination, file(prefix+"/older", "archived"), file(prefix+"/newer", "archived"))
	if result.summary != (restore.Summary{Restored: 1, Skipped: 1, Failed: 0}) ||
		!strings.Contains(result.output.String(), "skipped "+destination+"/newer: destination is newer") {
		t.Fatal(result.summary, result.output.String())
	}

	if readFile(t, filepath.Join(destination, "older")) != "archived" ||
		readFile(t, filepath.Join(destination, "newer")) != "local" {
		t.Fatal("newer policy replaced the wrong file")
	}
}

func TestApplyReportsTypeConflicts(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()

	err := os.Mkdir(filepath.Join(destination, configName), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(destination, "sub"), "file", 0o644, committed)

	result := apply(t, restore.Always, destination,
		file(prefix+"/"+configName, configBody), directory(prefix+"/sub"), file(prefix+"/sub/inner", "inner"))
	if result.summary != (restore.Summary{Restored: 0, Skipped: 0, Failed: 3}) ||
		strings.Count(result.output.String(), ": type conflict") != 2 {
		t.Fatal(result.summary, result.output.String())
	}

	info, err := os.Lstat(filepath.Join(destination, configName))
	if err != nil || !info.IsDir() {
		t.Fatal("conflicting directory was replaced", err)
	}
}

func TestApplyRejectsEscapingAndUnsupportedEntries(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	destination := filepath.Join(parent, "dest")

	err := os.Mkdir(destination, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	result := apply(t, restore.Always, destination,
		file(prefix+"/../../escape", "x"),
		fifo(prefix+"/fifo"),
		file(prefix+"/ok", "ok"),
	)
	if result.summary != (restore.Summary{Restored: 1, Skipped: 0, Failed: 1}) ||
		!strings.Contains(result.output.String(), ": unsupported entry type") {
		t.Fatal(result.summary, result.output.String())
	}

	_, err = os.Lstat(filepath.Join(parent, "escape"))
	if err == nil {
		t.Fatal("entry escaped the destination")
	}
}

func TestApplySingleEntryUsesName(t *testing.T) {
	t.Parallel()

	destination := filepath.Join(t.TempDir(), "restored.conf")

	var output bytes.Buffer

	single := options(restore.Off, destination, &output)
	single.Prefix = prefix + "/" + configName

	summary, err := restore.Apply(t.Context(), buildTar(t, file(prefix+"/"+configName, configBody)), single)
	if err != nil || summary != (restore.Summary{Restored: 1, Skipped: 0, Failed: 0}) ||
		output.String() != "restored "+destination+"\n" {
		t.Fatal(summary, output.String(), err)
	}

	if readFile(t, destination) != configBody {
		t.Fatal("single entry was not written under the requested name")
	}
}

// git archive names its global header pax_global_header, like an archived root entry of that name could be.
func TestApplyRestoresEntryNamedLikeTheGlobalHeader(t *testing.T) {
	t.Parallel()

	destination := filepath.Join(t.TempDir(), "restored")

	var output bytes.Buffer

	single := options(restore.Off, destination, &output)
	single.Prefix = globalHeader

	summary, err := restore.Apply(t.Context(), buildTar(t, file(globalHeader, configBody)), single)
	if err != nil || summary != (restore.Summary{Restored: 1, Skipped: 0, Failed: 0}) {
		t.Fatal(summary, output.String(), err)
	}

	if readFile(t, destination) != configBody {
		t.Fatal("entry named like the global header was not restored")
	}
}

func TestApplyDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	destination := filepath.Join(parent, "missing")

	var output bytes.Buffer

	dry := options(restore.Off, destination, &output)
	dry.DryRun = true

	summary, err := restore.Apply(t.Context(), buildTar(t, file(prefix+"/"+configName, configBody)), dry)
	if err != nil || summary != (restore.Summary{Restored: 1, Skipped: 0, Failed: 0}) ||
		output.String() != "restored "+destination+"/"+configName+"\n" {
		t.Fatal(summary, output.String(), err)
	}

	_, err = os.Lstat(destination)
	if err == nil {
		t.Fatal("dry run created the destination")
	}
}

func TestApplyDryRunEvaluatesPolicyAgainstExistingRoot(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()
	writeFile(t, filepath.Join(destination, configName), "local", 0o644, committed)

	var output bytes.Buffer

	dry := options(restore.Off, destination, &output)
	dry.DryRun = true

	summary, err := restore.Apply(t.Context(), buildTar(t, file(prefix+"/"+configName, configBody)), dry)
	if err != nil || summary != (restore.Summary{Restored: 0, Skipped: 1, Failed: 0}) {
		t.Fatal(summary, output.String(), err)
	}
}

func TestApplyReusesExistingDirectoriesSilently(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()

	err := os.Mkdir(filepath.Join(destination, "sub"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	result := apply(t, restore.Off, destination, directory(prefix+"/sub"), file(prefix+"/sub/inner", "inner"))
	if result.summary != (restore.Summary{Restored: 1, Skipped: 0, Failed: 0}) ||
		strings.Count(result.output.String(), "\n") != 1 {
		t.Fatal(result.summary, result.output.String())
	}

	info, err := os.Lstat(filepath.Join(destination, "sub"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("existing directory mode changed", info.Mode(), err)
	}
}

func TestApplyReportsMissingPrefix(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	destination := filepath.Join(parent, "dest")

	var output bytes.Buffer

	absent := options(restore.Off, destination, &output)
	absent.Prefix = "etc/absent"

	_, err := restore.Apply(t.Context(), buildTar(t, file(prefix+"/"+configName, configBody)), absent)
	if !errors.Is(err, restore.ErrSourceMissing) || output.Len() != 0 {
		t.Fatal(output.String(), err)
	}

	_, err = os.Lstat(destination)
	if err == nil {
		t.Fatal("a missing prefix created the destination")
	}
}

func TestApplyCreatesDirectoryDestination(t *testing.T) {
	t.Parallel()

	destination := filepath.Join(t.TempDir(), "nested", "dest")
	result := apply(t, restore.Off, destination, file(prefix+"/"+configName, configBody))

	if result.summary != (restore.Summary{Restored: 1, Skipped: 0, Failed: 0}) ||
		readFile(t, filepath.Join(destination, configName)) != configBody {
		t.Fatal(result.summary, result.output.String())
	}
}

func TestParsePolicy(t *testing.T) {
	t.Parallel()

	policies := map[string]restore.Policy{"off": restore.Off, "always": restore.Always, "newer": restore.Newer}
	for value, want := range policies {
		policy, err := restore.ParsePolicy(value)
		if err != nil || policy != want {
			t.Fatal(value, policy, err)
		}
	}

	_, err := restore.ParsePolicy("maybe")
	if err == nil {
		t.Fatal("unknown policy accepted")
	}
}
