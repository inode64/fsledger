package notify

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
)

// Sample evidence shaped like the messages the daemon queues, so checked templates meet real fields.
const (
	sampleChangeID = "PBHEY4LZLTYDYX3O7OYWIBYK3H"
	sampleMergedID = "QXKZ2M7TJ4WVN5RCA3LDYHF6GE"
	sampleObserved = 1792138666102146517
	// sampleBackdated is how long before its birth the backdated samples were last modified.
	sampleBackdated = time.Duration(1792138666102146517 - 1697000000000000000)
	sampleModified  = "modified"
	sampleAdded     = "added"
	sampleDeleted   = "deleted"
	sampleSize      = "size"
	sampleRegular   = `"regular"`
	sampleNoType    = `""`
	sampleOldHash   = "sha256:55f2c0e6a1b9d7f4e3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c6"
	sampleNewHash   = "sha256:7db5a4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5"
	sampleActor     = "root (0): vim modified files\n\nUser:\n  real: root (0)\n  login: admin (1000)\n" +
		"  effective: root (0)\n\nProcess:\n  pid: 3785335\n  start_time: 17173045\n" +
		"  executable: \"/usr/bin/vim\"\n\nDetection:\n  backend: fanotify"
	sampleReconciled = "unknown: filesystem changes detected\n\nActor: unknown\nDetection: reconciliation\n" +
		"Reason: overflow or retry"
	sampleReason = "fanotify directory left watched coverage; reconciliation required"
)

type templateSample struct {
	label   string
	message catalog.Message
}

// CheckTemplates executes the custom template of every notifier against sample messages of each event
// a repository routes to it. Parsing resolves neither fields nor indexes, so a template that only
// parses could fail every delivery, which the outbox would retry without ever sending the alert.
// Configuration cannot run this itself: it cannot import catalog messages. Reports use their own
// renderer, and the built-in template is covered by tests.
func CheckTemplates(cfg *config.Config) error {
	samples := sync.OnceValue(templateSamples)

	for _, name := range slices.Sorted(maps.Keys(cfg.Notifiers)) {
		notifier := cfg.Notifiers[name]
		if notifier.Template == "" {
			continue
		}

		parsed, exists := cfg.Templates[notifier.Template]
		if !exists || parsed.Subject == nil || parsed.Body == nil {
			return fault.New("invalid notification template " + notifier.Template + " for notifier " + name)
		}

		for _, sample := range samples() {
			if !routed(cfg, name, sample.message.Event) {
				continue
			}

			err := checkTemplate(parsed, sample.message)
			if err != nil {
				return fault.Wrap(
					fmt.Sprintf("notifier %s: template %s fails for %s", name, notifier.Template, sample.label),
					err,
				)
			}
		}
	}

	return nil
}

// routed reports whether some repository sends the event to the notifier as an immediate alert.
func routed(cfg *config.Config, notifier, event string) bool {
	for name := range cfg.Repositories {
		settings := cfg.Repositories[name].Notifications
		if slices.Contains(settings.Use, notifier) && slices.Contains(settings.Events, event) {
			return true
		}
	}

	return false
}

// checkTemplate applies the rules of render that depend on the template rather than on message size,
// keeping the text/template error, which names the failing part, line and action.
func checkTemplate(parsed config.Template, message catalog.Message) error {
	var subject strings.Builder

	err := parsed.Subject.Execute(&subject, message)
	if err != nil {
		return fault.Wrap("render subject", err)
	}

	if strings.ContainsAny(subject.String(), "\r\n") {
		return fault.New("notification subject contains a line break")
	}

	return fault.Wrap("render body", parsed.Body.Execute(io.Discard, message))
}

// templateSamples covers each message shape a template receives: a detailed change; a change above
// the detail limit, merged from two operations so it has no common change ID; and operational events,
// which carry a reason but no paths.
func templateSamples() []templateSample {
	// This modification also gains a file capability and has its mtime moved back before its birth, so
	// templates render attribute values and the backdating hint of a modified file.
	modified := sampleDetail("/usr/local/bin/backup", sampleModified, sampleActor, sampleChangeID,
		sampleField(integrity.FieldHash, sampleOldHash, sampleNewHash),
		sampleField(integrity.FieldMode, "33188", "33261"),
		sampleField(integrity.FieldXattrs, "null (available)",
			`[{"name":"c2VjdXJpdHkuY2FwYWJpbGl0eQ==","value":"AAAAAgAgAAAAAAAAAAAAAAAAAAA="}] (available)`),
		sampleField(integrity.FieldMtime, "1792138600000000000", "1697000000000000000"))
	modified.BackdatedBy = sampleBackdated
	// Creations and deletions carry a snapshot of the version that exists; this creation keeps an mtime
	// years before its birth time, so templates also render the backdating hint.
	added := sampleDetail("/etc/nginx/conf.d/site.conf", sampleAdded, sampleReconciled, sampleChangeID,
		sampleField(integrity.FieldType, sampleNoType, sampleRegular),
		sampleField(integrity.FieldHash, ":", sampleNewHash),
		sampleField(sampleSize, "0", "9475"),
		sampleField(integrity.FieldMode, "0", "33188"),
		sampleField(integrity.FieldUID, "0", "0"),
		sampleField(integrity.FieldGID, "0", "0"),
		sampleField("nlink", "0", "1"),
		sampleField(integrity.FieldMtime, "0", "1697000000000000000"),
		sampleField("ctime", "0", "1792138666102146517"),
		sampleField("btime", "0 (known=false)", "1792138666102146517 (known=true)"),
		sampleField(integrity.FieldXattrs, "null ()",
			`[{"name":"dXNlci5taW1lX3R5cGU=","value":"dGV4dC9wbGFpbg=="}] (available)`))
	added.BackdatedBy = sampleBackdated
	deleted := sampleDetail("/etc/nginx/old.conf", sampleDeleted, sampleActor, sampleChangeID,
		sampleField(integrity.FieldType, sampleRegular, sampleNoType),
		sampleField(integrity.FieldHash, sampleOldHash, ":"),
		sampleField(sampleSize, "512", "0"),
		sampleField(integrity.FieldMode, "33188", "0"),
		sampleField(integrity.FieldMtime, "1792138600000000000", "0"))

	grouped := make([]catalog.Detail, catalog.Message{}.DetailLimit()+1)
	for index := range grouped {
		actor, identifier := sampleActor, sampleChangeID
		if index >= len(grouped)/2 {
			actor, identifier = sampleReconciled, sampleMergedID
		}

		grouped[index] = sampleDetail(fmt.Sprintf("/var/www/htdocs/wp-content/plugins/cache/file-%04d.php", index),
			sampleAdded, actor, identifier, sampleField(sampleSize, "0", "1024"))
	}

	return []templateSample{
		{label: config.NotificationChange + " events", message: sampleMessage(
			config.NotificationChange, sampleChangeID, "", modified, added, deleted,
		)},
		{
			label:   fmt.Sprintf("%s events of more than %d paths", config.NotificationChange, len(grouped)-1),
			message: sampleMessage(config.NotificationChange, "", "", grouped...),
		},
		{label: config.NotificationError + " events", message: sampleMessage(
			config.NotificationError, sampleChangeID, sampleReason,
		)},
		{label: config.NotificationRecovery + " events", message: sampleMessage(
			config.NotificationRecovery, sampleChangeID, sampleReason,
		)},
	}
}

func sampleField(field, before, after string) catalog.FieldChange {
	return catalog.FieldChange{Field: field, Before: before, After: after}
}

func sampleDetail(path, kind, actor, identifier string, fields ...catalog.FieldChange) catalog.Detail {
	return catalog.Detail{
		ChangeID: identifier, Kind: kind, Actor: actor, Path: []byte(path),
		Fields: fields, Observed: sampleObserved, BackdatedBy: 0,
	}
}

func sampleMessage(event, identifier, reason string, details ...catalog.Detail) catalog.Message {
	paths := make([]string, len(details))
	for index := range details {
		paths[index] = string(details[index].Path)
	}

	return catalog.Message{
		Report: nil, Repository: "system", Host: "web01", Hostname: "web01.example.org", Event: event,
		ChangeID: identifier, Reason: reason, Paths: paths, Details: details, Count: len(details),
	}
}
