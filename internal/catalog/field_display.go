package catalog

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/inode64/fsledger/internal/humanize"
	"github.com/inode64/fsledger/internal/integrity"
)

// DisplayFields renders VisibleFields for people: timestamps in local time, sizes in human units,
// modes as ls-style permissions with the raw st_mode in hex, UID/GID with locally resolved names,
// symlink targets as escaped text, xattrs and ACLs as name=value, and the absent side of a creation
// or deletion as "—". It leaves out what carries no evidence for a reader: has_btime (part of btime),
// device, nlink unless a regular file has more than one link, and attribute readability unless
// reading failed. VisibleFields, classification and stored evidence are unchanged.
func (detail Detail) DisplayFields() []FieldChange {
	return displayFields(detail.VisibleFields(), detail.Kind)
}

func displayFields(fields []FieldChange, kind string) []FieldChange {
	view := fieldView{kind: kind, fileType: presentType(fields, kind), statusShown: false}
	display := make([]FieldChange, 0, len(fields))

	for _, field := range fields {
		if shown, visible := view.show(field); visible {
			display = append(display, shown)
		}
	}

	return display
}

// fieldView decides per field what a reader sees; attribute readability is reported at most once.
type fieldView struct {
	kind, fileType string
	statusShown    bool
}

func (view *fieldView) show(field FieldChange) (FieldChange, bool) {
	switch field.Field {
	case fieldHasBtime, fieldDevice:
		return field, false
	case fieldNlink:
		return presentable(field, view.kind), extraLinks(field, view.kind, view.fileType)
	case fieldAttributesStatus:
		return view.status(field)
	case fieldXattrs, fieldACL:
		beforeValue, beforeStatus := splitStatus(field.Before)
		afterValue, afterStatus := splitStatus(field.After)

		if beforeValue == afterValue {
			// The existing version has no attributes: report only a failed read, and only once.
			return view.status(FieldChange{Field: fieldAttributesStatus, Before: beforeStatus, After: afterStatus})
		}
	}

	return presentable(field, view.kind), true
}

func (view *fieldView) status(field FieldChange) (FieldChange, bool) {
	if view.statusShown || !readFailed(field, view.kind) {
		return field, false
	}

	view.statusShown = true

	return presentable(field, view.kind), true
}

// presentable humanizes each existing side and marks the side that does not exist as absent, without
// looking it up.
func presentable(field FieldChange, kind string) FieldChange {
	before, after := humanize.Absent, humanize.Absent
	if kind != changeAdded {
		before = displayValue(field.Field, field.Before)
	}

	if kind != changeDeleted {
		after = displayValue(field.Field, field.After)
	}

	return FieldChange{Field: field.Field, Before: before, After: after}
}

// presentValues returns the values that describe an existing file version.
func presentValues(field FieldChange, kind string) []string {
	switch kind {
	case changeAdded:
		return []string{field.After}
	case changeDeleted:
		return []string{field.Before}
	default:
		return []string{field.Before, field.After}
	}
}

// currentValue is the latest existing version: the deleted one for deletions, otherwise the new one.
func currentValue(field FieldChange, kind string) string {
	if kind == changeDeleted {
		return field.Before
	}

	return field.After
}

func presentType(fields []FieldChange, kind string) string {
	for _, field := range fields {
		if field.Field == fieldType {
			return unquoted(currentValue(field, kind))
		}
	}

	return ""
}

// extraLinks reports a regular file (or one of unknown type) with more than one hard link: another
// name shares its contents, which matters for a monitored tree. Directories always have several.
func extraLinks(field FieldChange, kind, fileType string) bool {
	if fileType != "" && fileType != integrity.TypeRegular {
		return false
	}

	for _, value := range presentValues(field, kind) {
		links, err := strconv.ParseUint(value, 10, 64)
		if err == nil && links > 1 {
			return true
		}
	}

	return false
}

// readFailed reports whether an existing version could not have its attributes read. A filesystem
// without attribute support is not a failure: those attributes are simply not shown.
func readFailed(field FieldChange, kind string) bool {
	for _, value := range presentValues(field, kind) {
		status := unquoted(value)
		if status != "" && status != integrity.AttributesAvailable && integrity.AttributesSupported(status) {
			return true
		}
	}

	return false
}

// splitStatus separates a value from its " (available)" style readability suffix.
func splitStatus(value string) (string, string) {
	trimmed, found := strings.CutSuffix(value, suffixClosing)
	if !found {
		return value, ""
	}

	before, status, found := strings.CutLast(trimmed, suffixOpening)
	if !found {
		return value, ""
	}

	return before, status
}

// unquoted decodes a JSON string value and returns any other value unchanged.
func unquoted(value string) string {
	var text string

	if json.Unmarshal([]byte(value), &text) == nil {
		return text
	}

	return value
}

func displayValue(name, value string) string {
	switch name {
	case fieldMode:
		return displayMode(value)
	case fieldUID:
		return displayUID(value)
	case fieldGID:
		return displayGID(value)
	case fieldType, fieldAttributesStatus:
		return displayText(value)
	case fieldHash:
		return displayHash(value)
	case fieldTarget:
		return displayTarget(value)
	case fieldXattrs, fieldACL:
		return displayAttributes(value)
	case fieldMtime, fieldCtime, fieldAtime:
		return displayTime(value, parseTime)
	case fieldBtime:
		return displayTime(value, parseBtime)
	case fieldSize:
		return displaySize(value)
	default:
		return value
	}
}

func parseTime(value string) (int64, bool) {
	nanoseconds, err := strconv.ParseInt(value, 10, 64)

	return nanoseconds, err == nil
}

// parseBtime reads a birth time the filesystem provides; an unknown one carries no time.
func parseBtime(value string) (int64, bool) {
	number, found := strings.CutSuffix(value, knownTrue)
	if !found {
		return 0, false
	}

	return parseTime(number)
}

func displayTime(value string, parse func(string) (int64, bool)) string {
	nanoseconds, ok := parse(value)
	if !ok {
		return value
	}

	return humanize.Time(nanoseconds)
}

func displaySize(value string) string {
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return value
	}

	return humanize.Size(size)
}

func displayMode(value string) string {
	mode, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return value
	}

	return humanize.Mode(uint32(mode))
}

// displayText shows a stored string such as a file type or attribute status, or "—" when empty.
func displayText(value string) string {
	text := unquoted(value)
	if text == "" {
		return humanize.Absent
	}

	return humanize.Printable(text)
}

func displayHash(value string) string {
	if value == "" || value == hashSeparator {
		return humanize.Absent
	}

	return value
}
