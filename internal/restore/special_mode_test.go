package restore_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/restore"
)

func TestApplyAlwaysKeepsSpecialModeBits(t *testing.T) {
	t.Parallel()

	for _, scenario := range []struct {
		name string
		mode fs.FileMode
	}{
		{name: "setuid and setgid", mode: 0o755 | fs.ModeSetuid | fs.ModeSetgid},
		{name: "sticky", mode: 0o644 | fs.ModeSticky},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()

			destination := t.TempDir()
			target := filepath.Join(destination, configName)
			writeFile(t, target, "local", 0o600, committed.Add(-time.Hour))

			err := os.Chmod(target, scenario.mode)
			if err != nil {
				t.Fatal(err)
			}

			result := apply(t, restore.Always, destination, executable(prefix+"/"+configName, configBody))
			if result.summary != (restore.Summary{Restored: 1, Skipped: 0, Failed: 0}) {
				t.Fatal(result.summary, result.output.String())
			}

			info, err := os.Lstat(target)
			if err != nil || info.Mode() != scenario.mode {
				t.Fatal(info.Mode(), err)
			}

			if readFile(t, target) != configBody {
				t.Fatal("content was not replaced")
			}
		})
	}
}
