package cli_test

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

const versionFlag = "--version"

// Version must not depend on a readable configuration, including through fsledgerd's implicit run.
func TestVersionNeedsNoConfiguration(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing.yaml")

	for _, args := range [][]string{
		{versionFlag},
		{"-version"},
		{"version"},
		{"run", versionFlag},
		{checkCommand, "-c", missing, versionFlag},
		{"baseline", versionFlag, "init"},
		{"migrate_aide", versionFlag},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			err := cli.Run(args, &stdout, &stderr)
			if err != nil {
				t.Fatalf("Run(%q) error = %v; stderr %q", args, err, stderr.String())
			}

			output := stdout.String()
			if !strings.HasPrefix(output, "fsledger ") || !strings.Contains(output, runtime.Version()) {
				t.Fatalf("Run(%q) output = %q; want program name and Go version", args, output)
			}

			if strings.Count(output, "\n") != 1 || stderr.Len() != 0 {
				t.Fatalf("Run(%q) output = %q, stderr = %q; want one stdout line", args, output, stderr.String())
			}
		})
	}
}
