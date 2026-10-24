// Package doctor reports real per-mount capabilities without starting a daemon.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/attribution/audit"
	"github.com/inode64/fsledger/internal/capabilities"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
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

	report := Report{Kernel: unix.ByteSliceToString(uts.Release[:]), Audit: auditStatus(cfg)}
	report.Missing, report.Skipped = map[string][]string{}, map[string][]string{}

	resolved := cfg.ResolveSources()
	for _, name := range cfg.Names() {
		matcher, matcherErr := cfg.Matcher(name)
		if matcherErr != nil {
			return Report{}, matcherErr
		}

		effective := cfg.ForRepository(name)
		for _, root := range resolved[name].Roots {
			paths := inspectRoot(effective, matcher, name, root, mounts)
			report.Paths = append(report.Paths, paths...)
			report.Warnings = append(report.Warnings, attributeWarnings(name, effective.Integrity, paths)...)
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

// attributeWarnings are the advisories the daemon keeps in the repository status once it starts.
func attributeWarnings(name string, policy config.Integrity, paths []Path) []string {
	var warnings []string

	for _, path := range paths {
		for _, message := range integrity.AttributeAdvisories(policy, path.Path, path.Mount.Type) {
			warnings = append(warnings, name+": "+message)
		}
	}

	return warnings
}

// auditStatus follows the daemon, which subscribes only for repositories whose effective settings
// enable Audit with an event backend; the global section is just their default.
func auditStatus(cfg *config.Config) audit.Status {
	status := audit.Probe()

	for _, name := range cfg.Names() {
		repo := cfg.Repositories[name]
		if repo.AuditEnabled() {
			return status
		}
	}

	status.Usable = false
	status.Reason = "disabled in configuration"

	return status
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

// inspectRoot skips excluded mount points as the daemon does, so they are neither listed nor probed.
func inspectRoot(cfg *config.Config, matcher *exclude.Matcher, name, root string, mounts []mountinfo.Mount) []Path {
	resolved := mountinfo.Resolve(root, mounts)
	if len(resolved) == 0 {
		// The daemon watches such a root as one boundary of unknown type; so does its diagnosis.
		resolved = []mountinfo.Mount{{Point: root, Type: event.Unknown}}
	}

	result := make([]Path, 0, len(resolved))
	for index, mount := range resolved {
		path := root
		if index > 0 {
			path = mount.Point
		}

		if matcher.Match(path) {
			continue
		}

		// The daemon walks each boundary with its nested mount points protected; so does the diagnosis.
		result = append(
			result,
			inspectPath(cfg, matcher.Protect(mountinfo.Nested(path, resolved)...), name, path, mount),
		)
	}

	return result
}

// installedMode asks the watcher itself which representation it can install on this path, so the
// diagnosis cannot drift from what the daemon will do. Capabilities only explain each separate flag.
// Like the daemon, it installs the recursive marks too: a directory below the root that cannot be
// marked makes the daemon fall back, although the group and its root mark worked.
func installedMode(matcher *exclude.Matcher, path string) (string, bool) {
	detector, err := fanotify.New(path, matcher)
	if err != nil {
		return "", false
	}
	defer resource.Close(detector)

	err = detector.Start(context.Background())
	if err != nil {
		return "", false
	}

	return detector.Mode(), detector.PIDFD()
}

func inspectPath(cfg *config.Config, matcher *exclude.Matcher, name, path string, mount mountinfo.Mount) Path {
	var caps capabilities.Capabilities
	if cfg.Watch.Backend != event.Polling {
		caps = capabilities.Probe(path)
	}

	detector, attribution := event.Polling, event.Unknown
	mode := ""
	pidfd := false

	if caps.Inotify.Available {
		detector = event.Inotify
	}

	if cfg.Watch.Backend == config.BackendAuto || cfg.Watch.Backend == event.Fanotify {
		mode, pidfd = installedMode(matcher, path)
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
