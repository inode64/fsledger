// Package buildinfo identifies the running build for the CLI and for delivered notifications.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// version is injected by make build with -ldflags -X; empty falls back to Go build metadata.
var version string

const revisionLength = 12

// Release names the build: the injected version, else the module version, else "devel".
func Release() string {
	release, _ := identity()

	return release
}

// Line is the full --version output, which also names the commit and Go toolchain.
func Line() string {
	release, revision := identity()

	details := runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH
	if revision != "" {
		details = "commit " + revision + ", " + details
	}

	return "fsledger " + release + " (" + details + ")"
}

func identity() (string, string) {
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

	return release, revision
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
