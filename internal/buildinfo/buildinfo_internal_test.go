package buildinfo

import (
	"runtime"
	"strings"
	"testing"
)

//nolint:paralleltest // The injected version is package state shared with every caller.
func TestReleaseFollowsInjectedVersion(t *testing.T) {
	injected := version

	t.Cleanup(func() { version = injected })

	version = ""

	if release := Release(); release != "devel" {
		t.Fatalf("Release() without injection = %q; want devel", release)
	}

	version = "release-test"

	if release := Release(); release != "release-test" {
		t.Fatalf("Release() = %q; want the injected version", release)
	}

	line := Line()
	if !strings.HasPrefix(line, "fsledger release-test (") || !strings.Contains(line, runtime.Version()) {
		t.Fatalf("Line() = %q; want program, release and Go version", line)
	}
}
