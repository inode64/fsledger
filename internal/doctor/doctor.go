// Package doctor reports real per-mount capabilities without starting a daemon.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/inode64/fsledger/internal/fault"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/attribution/audit"
	"github.com/inode64/fsledger/internal/capabilities"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher/fanotify"
)

// Path reports a source that currently exists, or one of its nested mounts.
type Path struct {
	Repository         string                    `json:"repository"`
	Path               string                    `json:"path"`
	Detector           string                    `json:"detector"`
	Attribution        string                    `json:"attribution"`
	FanotifyMode       string                    `json:"fanotify_mode,omitempty"`
	Capabilities       capabilities.Capabilities `json:"capabilities"`
	Mount              mountinfo.Mount           `json:"mount"`
	CapabilitiesProbed bool                      `json:"capabilities_probed"`
	Reconciliation     bool                      `json:"reconciliation"`
	FanotifyPIDFD      bool                      `json:"fanotify_pidfd"`
}

// Report describes this process's namespace and privileges, not hypothetical root access.
// Missing and Skipped list, by repository, configured sources that were not probed.
type Report struct {
	FanotifyPIDFDLifetime *fanotify.PIDFDLifetime `json:"fanotify_pidfd_lifetime,omitempty"`
	Warnings              []string                `json:"warnings,omitempty"`
	Missing               map[string][]string     `json:"missing_sources,omitempty"`
	Skipped               map[string][]string     `json:"skipped_sources,omitempty"`
	Kernel                string                  `json:"kernel"`
	Audit                 audit.Status            `json:"audit"`
	Paths                 []Path                  `json:"paths"`
}

// Inspect probes each resolved source and nested mount; absent or skipped entries are only listed.
func Inspect(cfg *config.Config) (Report, error) {
	mounts, err := mountinfo.Read()
	if err != nil {
		return Report{}, err
	}

	var uts unix.Utsname

	err = unix.Uname(&uts)
	if err != nil {
		return Report{}, fault.Wrap("read kernel release", err)
	}

	report := Report{Kernel: unix.ByteSliceToString(uts.Release[:]), Audit: audit.Probe()}
	if !cfg.Attribution.Audit.Enabled {
		report.Audit.Usable = false
		report.Audit.Reason = "disabled in configuration"
	}

	report.Missing, report.Skipped = map[string][]string{}, map[string][]string{}

	resolved := cfg.ResolveSources()
	for _, name := range cfg.Names() {
		for _, root := range resolved[name].Roots {
			report.Paths = append(report.Paths, inspectRoot(cfg.ForRepository(name), name, root, mounts)...)
		}

		if len(resolved[name].Missing) > 0 {
			report.Missing[name] = resolved[name].Missing
		}

		if len(resolved[name].Skipped) > 0 {
			report.Skipped[name] = resolved[name].Skipped
		}
	}

	report.probePIDFDLifetime(cfg.Runtime)

	return report, nil
}

func (report *Report) probePIDFDLifetime(runtime string) {
	for _, path := range report.Paths {
		if path.FanotifyPIDFD {
			result := fanotify.ProbePIDFDLifetime(context.Background(), runtime)

			report.FanotifyPIDFDLifetime = &result
			if warning := result.Warning(); warning != "" {
				report.Warnings = append(report.Warnings, warning)
			}

			break
		}
	}
}

// Print writes a stable machine-readable diagnostic with explicit failure reasons.
func Print(writer io.Writer, cfg *config.Config) error {
	report, err := Inspect(cfg)
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")

	err = encoder.Encode(report)
	if err != nil {
		return fmt.Errorf("write doctor: %w", err)
	}

	return nil
}

func inspectRoot(cfg *config.Config, name, root string, mounts []mountinfo.Mount) []Path {
	resolved := mountinfo.Resolve(root, mounts)

	result := make([]Path, 0, len(resolved))
	for index, mount := range resolved {
		path := root
		if index > 0 {
			path = mount.Point
		}

		result = append(result, inspectPath(cfg, name, path, mount))
	}

	return result
}

// installedMode asks the watcher itself which representation it can install on this path, so the
// diagnosis cannot drift from what the daemon will do. Capabilities only explain each separate flag.
func installedMode(cfg *config.Config, name, path string) (string, bool) {
	matcher, err := cfg.Matcher(name)
	if err != nil {
		return "", false
	}

	detector, err := fanotify.New(path, matcher)
	if err != nil {
		return "", false
	}
	defer resource.Close(detector)

	return detector.Mode(), detector.PIDFD()
}

func inspectPath(cfg *config.Config, name, path string, mount mountinfo.Mount) Path {
	var caps capabilities.Capabilities
	if cfg.Watch.Backend != event.Polling {
		caps = capabilities.Probe(path)
	}

	detector, attribution := event.Polling, event.Unknown
	mode := ""
	pidfd := false

	if cfg.Watch.Backend != event.Polling && caps.Inotify.Available {
		detector = event.Inotify
	}

	if cfg.Watch.Backend == config.BackendAuto || cfg.Watch.Backend == event.Fanotify {
		mode, pidfd = installedMode(cfg, name, path)
	}

	if mode != "" {
		detector = event.Fanotify + "+" + detector
		attribution = "fanotify+proc (best effort)"
	}

	return Path{
		CapabilitiesProbed: cfg.Watch.Backend != event.Polling,
		FanotifyPIDFD:      pidfd,
		FanotifyMode:       mode,
		Repository:         name,
		Path:               path,
		Mount:              mount,
		Capabilities:       caps,
		Detector:           detector,
		Attribution:        attribution,
		Reconciliation:     cfg.Watch.Reconcile.Enabled || mount.Remote() || strings.HasSuffix(detector, event.Polling),
	}
}
