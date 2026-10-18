package catalog

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/inode64/fsledger/internal/humanize"
	"github.com/inode64/fsledger/internal/integrity"
)

const (
	// availableSuffix marks attributes that were readable.
	availableSuffix = suffixOpening + integrity.AttributesAvailable + suffixClosing
	// untrackedTime is how a timestamp the filesystem does not keep is stored.
	untrackedTime = "0"
	bytesUnit     = " B"
	// backdatedThreshold ignores clock granularity and short copy delays; real backdating spans far more.
	backdatedThreshold = time.Hour
)

// DisplayFields renders VisibleFields for people: timestamps in local time, sizes in human units,
// modes as ls-style permissions with the raw st_mode in hex, UID/GID with locally resolved names,
// and the absent side of a creation or deletion as "—". It leaves out what carries no evidence for
// a reader: has_btime (part of btime),
// device in creations and deletions, nlink unless a regular file has more than one link, and
// attribute readability unless reading failed. VisibleFields, classification and stored evidence
// are unchanged.
func (detail Detail) DisplayFields() []FieldChange {
	return displayFields(detail.VisibleFields(), detail.Kind)
}

// DisplayBaseline renders the approved-reference comparison the same way.
func (detail Detail) DisplayBaseline() []FieldChange {
	return displayFields(detail.VisibleBaseline(), "")
}

// BackdatedBy returns how much earlier the modification time of the current version is than its
// birth time, or zero. Copies that keep times (tar, rsync -a, cp -p) produce it as well as a
// backdated touch, so templates present it as a hint rather than a verdict.
func (detail Detail) BackdatedBy() time.Duration {
	var mtime, btime timeValue

	haveMtime, haveBtime := false, false

	for _, field := range detail.Fields {
		switch field.Field {
		case fieldMtime:
			mtime, haveMtime = parseTime(currentValue(field, detail.Kind))
		case fieldBtime:
			btime, haveBtime = parseBtime(currentValue(field, detail.Kind))
		}
	}

	if !haveMtime || !haveBtime || !btime.known || mtime.nanoseconds == 0 || btime.nanoseconds == 0 {
		return 0
	}

	if gap := time.Duration(btime.nanoseconds - mtime.nanoseconds); gap > backdatedThreshold {
		return gap
	}

	return 0
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
	if !view.supported(field) {
		return field, false
	}

	switch field.Field {
	case fieldHasBtime:
		return field, false
	case fieldDevice:
		// A snapshot always carries the device; only an explicitly compared change says something.
		return presentable(field, view.kind), view.kind != changeAdded && view.kind != changeDeleted
	case fieldNlink:
		return presentable(field, view.kind), extraLinks(field, view.kind, view.fileType)
	case fieldAttributesStatus:
		return view.status(field)
	case fieldXattrs, fieldACL:
		beforeValue, beforeStatus := splitStatus(field.Before)
		afterValue, afterStatus := splitStatus(field.After)

		if beforeValue == afterValue {
			// Only readability changed: report it once, and only when reading failed.
			return view.status(FieldChange{Field: fieldAttributesStatus, Before: beforeStatus, After: afterStatus})
		}
	}

	return presentable(field, view.kind), true
}

// supported reports whether every existing side of a field is evidence its filesystem provides: an
// access time the mount does not keep, an unknown birth time or unsupported attributes are not shown.
func (view *fieldView) supported(field FieldChange) bool {
	for _, value := range presentValues(field, view.kind) {
		if !supportedValue(field.Field, value) {
			return false
		}
	}

	return true
}

func supportedValue(name, value string) bool {
	switch name {
	case fieldAtime:
		return value != untrackedTime
	case fieldBtime:
		return !strings.HasSuffix(value, knownFalse)
	case fieldXattrs, fieldACL:
		_, status := splitStatus(value)

		return integrity.AttributesSupported(status)
	case fieldAttributesStatus:
		return integrity.AttributesSupported(unquoted(value))
	default:
		return true
	}
}

func (view *fieldView) status(field FieldChange) (FieldChange, bool) {
	if view.statusShown || !readFailed(field, view.kind) {
		return field, false
	}

	view.statusShown = true

	return presentable(field, view.kind), true
}

// presentable humanizes both values and marks the side that does not exist as absent.
func presentable(field FieldChange, kind string) FieldChange {
	before, after := displayValues(field)

	switch kind {
	case changeAdded:
		before = humanize.Absent
	case changeDeleted:
		after = humanize.Absent
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

func displayValues(field FieldChange) (string, string) {
	switch field.Field {
	case fieldMtime, fieldCtime, fieldAtime:
		return displayTimes(field.Before, field.After, parseTime)
	case fieldBtime:
		return displayTimes(field.Before, field.After, parseBtime)
	case fieldSize:
		return displaySizes(field.Before, field.After)
	case fieldMode:
		return displayMode(field.Before), displayMode(field.After)
	case fieldUID:
		return displayUID(field.Before), displayUID(field.After)
	case fieldGID:
		return displayGID(field.Before), displayGID(field.After)
	case fieldType:
		return displayType(field.Before), displayType(field.After)
	case fieldHash:
		return displayHash(field.Before), displayHash(field.After)
	case fieldAttributesStatus:
		return displayStatus(field.Before), displayStatus(field.After)
	default:
		return field.Before, field.After
	}
}

type timeValue struct {
	nanoseconds int64
	known       bool
}

func parseTime(value string) (timeValue, bool) {
	nanoseconds, err := strconv.ParseInt(value, 10, 64)

	return timeValue{nanoseconds: nanoseconds, known: true}, err == nil
}

func parseBtime(value string) (timeValue, bool) {
	known := true

	number, found := strings.CutSuffix(value, knownTrue)
	if !found {
		number, found = strings.CutSuffix(value, knownFalse)
		known = false
	}

	if !found {
		return timeValue{}, false
	}

	parsed, ok := parseTime(number)
	parsed.known = known

	return parsed, ok
}

func displayTimes(before, after string, parse func(string) (timeValue, bool)) (string, string) {
	beforeTime, beforeOK := parse(before)
	afterTime, afterOK := parse(after)

	if !beforeOK || !afterOK {
		return before, after
	}

	// Unknown birth times never reach this point: fields a filesystem does not provide are hidden.
	render := func(value timeValue, precise bool) string {
		if precise {
			return humanize.PreciseTime(value.nanoseconds)
		}

		return humanize.Time(value.nanoseconds)
	}

	shownBefore, shownAfter := render(beforeTime, false), render(afterTime, false)
	if shownBefore == shownAfter {
		// Second precision would hide the difference that made this field visible.
		shownBefore, shownAfter = render(beforeTime, true), render(afterTime, true)
	}

	return shownBefore, shownAfter
}

func displaySizes(before, after string) (string, string) {
	beforeSize, beforeErr := strconv.ParseInt(before, 10, 64)
	afterSize, afterErr := strconv.ParseInt(after, 10, 64)

	if beforeErr != nil || afterErr != nil {
		return before, after
	}

	shownBefore, shownAfter := humanize.Size(beforeSize), humanize.Size(afterSize)
	if shownBefore == shownAfter {
		shownBefore += suffixOpening + before + bytesUnit + suffixClosing
		shownAfter += suffixOpening + after + bytesUnit + suffixClosing
	}

	return shownBefore, shownAfter
}

func displayMode(value string) string {
	mode, err := strconv.ParseUint(value, 10, 32)
	// Recorded modes are complete st_mode values; anything without a file type is shown as is.
	if err != nil || integrity.FileType(uint32(mode)) == "" {
		return value
	}

	return humanize.Mode(uint32(mode))
}

func displayType(value string) string {
	kind := unquoted(value)
	if kind == "" {
		return humanize.Absent
	}

	return humanize.Printable(kind)
}

func displayStatus(value string) string {
	status := unquoted(value)
	if status == "" {
		return humanize.Absent
	}

	return humanize.Printable(status)
}

func displayHash(value string) string {
	if value == "" || value == hashSeparator {
		return humanize.Absent
	}

	return value
}
