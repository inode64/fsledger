package cli_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/cli"
)

// Each manual verification compares the sources with the previous observation on the fixed policy:
// a new timestamp alone is no change, a new mode or content is.
func TestVerifyRecordsChangesOnFixedPolicy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	file, run := inventorySource(t, root, "{hash: {algorithm: blake3}}")

	later := time.Now().Add(time.Hour)

	err := os.Chtimes(file, later, later)
	if err != nil {
		t.Fatal(err)
	}

	if changed := verifiedChanges(t, run); changed != 0 {
		t.Fatalf("timestamps alone recorded %d changes", changed)
	}

	err = os.WriteFile(file, []byte("second"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if changed := verifiedChanges(t, run); changed != 1 {
		t.Fatalf("content change recorded %d changes, want 1", changed)
	}

	err = os.Chmod(file, 0o400)
	if err != nil {
		t.Fatal(err)
	}

	if changed := verifiedChanges(t, run); changed != 1 {
		t.Fatalf("mode change recorded %d changes, want 1", changed)
	}

	_, err = os.Stat(filepath.Join(root, fixtureState, fixtureRepository, ".git"))
	if !os.IsNotExist(err) {
		t.Fatal("DB repository contains Git")
	}
}

// Extended attributes count only when the repository selects them.
func TestVerifyComparesSelectedXattrs(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		integrity string
		want      int
	}{{"{}", 0}, {"{xattrs: true}", 1}} {
		file, run := inventorySource(t, t.TempDir(), fixture.integrity)

		err := unix.Setxattr(file, "user.fsledger", []byte("set"), 0)
		if errors.Is(err, unix.ENOTSUP) {
			t.Skip("temporary directory filesystem has no user xattrs")
		}

		if err != nil {
			t.Fatal(err)
		}

		if changed := verifiedChanges(t, run); changed != fixture.want {
			t.Fatalf("integrity %s recorded %d changes, want %d", fixture.integrity, changed, fixture.want)
		}
	}
}

// touch -d moves mtime back before the birth of the same file, which is a change on its own.
func TestVerifyRecordsBackdatedModificationTime(t *testing.T) {
	t.Parallel()

	file, run := inventorySource(t, t.TempDir(), "{}")

	var birth unix.Statx_t

	err := unix.Statx(unix.AT_FDCWD, file, 0, unix.STATX_BTIME, &birth)
	if err != nil {
		t.Fatal(err)
	}

	if birth.Mask&unix.STATX_BTIME == 0 {
		t.Skip("temporary directory filesystem has no birth times")
	}

	earlier := time.Now().Add(-48 * time.Hour)

	err = os.Chtimes(file, earlier, earlier)
	if err != nil {
		t.Fatal(err)
	}

	if changed := verifiedChanges(t, run); changed != 1 {
		t.Fatalf("backdated mtime recorded %d changes, want 1", changed)
	}
}

// A usage error must not leave a repository directory, Git metadata or a catalog behind.
func TestUsageErrorsCreateNoState(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"db", "git"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			invoke := inventoryRepository(t, root, "type: "+kind+"\npaths: ['"+root+"/source']\n")

			for name, args := range map[string][]string{
				"verify with an operand": {verifyCommand, "extra"},
				"unknown flag":           {verifyCommand, "--bogus", "id"},
				"unknown command":        {"bogus"},
			} {
				output, err := invoke(args...)
				if err == nil {
					t.Fatalf("%s accepted: %s", name, output)
				}

				_, err = os.Lstat(filepath.Join(root, fixtureState))
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s created repository state: %v", name, err)
				}
			}
		})
	}
}

// A removed or mistyped command is named as unknown, even when operands follow it.
func TestUnknownCommandWithOperands(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), fixtureMainYaml)

	for _, args := range [][]string{{"baseline", "init"}, {"changes"}, {"migrate_aide", "db", "repo"}} {
		err := cli.Run(append(args, "-c", missing), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), `unknown command "`+args[0]+`"`) {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// An option the command does not read must fail instead of being ignored.
func TestCommandsRejectOptionsTheyIgnore(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), fixtureMainYaml)

	for _, args := range [][]string{
		{"status", "--repository", "x"}, {"check", "--debug"}, {verifyCommand, "--effective"},
	} {
		err := cli.Run(append(args, "-c", missing), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "is not accepted by "+args[0]) {
			t.Errorf("%q: %v", args, err)
		}
	}
}
