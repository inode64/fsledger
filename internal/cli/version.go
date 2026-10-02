package cli

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"

	"github.com/inode64/fsledger/internal/fault"
)

// version is injected by make build with -ldflags -X; empty falls back to Go build metadata.
var version string

const revisionLength = 12

// printVersion never loads configuration so broken or missing YAML cannot hide the build identity.
func printVersion(stdout io.Writer) error {
	_, err := fmt.Fprintln(stdout, versionLine())

	return fault.Wrap("write version", err)
}

func versionLine() string {
	release, revision := version, ""

	info, available := debug.ReadBuildInfo()
	if available {
		if release == "" && info.Main.Version != "(devel)" {
			release = info.Main.Version
		}

		revision = vcsRevision(info.Settings)
	}

	if release == "" {
		release = "devel"
	}

	details := runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH
	if revision != "" {
		details = "commit " + revision + ", " + details
	}

	return "fsledger " + release + " (" + details + ")"
}

func vcsRevision(settings []debug.BuildSetting) string {
	revision, modified := "", false

	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}

	if revision == "" {
		return ""
	}

	revision = revision[:min(len(revision), revisionLength)]
	if modified {
		revision += "-dirty"
	}

	return revision
}
