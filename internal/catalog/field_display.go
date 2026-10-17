package catalog

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/inode64/fsledger/internal/humanize"
)

const (
	unknownValue  = "desconocido"
	absentHash    = ":"
	knownTrue     = " (known=true)"
	knownFalse    = " (known=false)"
	statusOpening = " ("
	fileTypeMask  = 0o170000
)

// DisplayFields renders VisibleFields for people: timestamps in local time, sizes and modes in
// human units, the absent side of a creation or deletion as "—", and availability-only changes
// (attributes_status, has_btime, xattrs or ACL that only became readable) left out.
// VisibleFields, classification and stored evidence are unchanged.
func (detail Detail) DisplayFields() []FieldChange {
	return displayFields(detail.VisibleFields(), detail.Kind)
}

// DisplayBaseline renders the approved-reference comparison the same way.
func (detail Detail) DisplayBaseline() []FieldChange {
	return displayFields(detail.VisibleBaseline(), "")
}

func displayFields(fields []FieldChange, kind string) []FieldChange {
	display := make([]FieldChange, 0, len(fields))

	for _, field := range fields {
		switch field.Field {
		case fieldAttributesStatus, fieldHasBtime:
			continue
		case fieldXattrs, fieldACL:
			if withoutStatus(field.Before) == withoutStatus(field.After) {
				continue
			}
		}

		before, after := displayValues(field)

		switch kind {
		case changeAdded:
			before = humanize.Absent
		case changeDeleted:
			after = humanize.Absent
		}

		display = append(display, FieldChange{Field: field.Field, Before: before, After: after})
	}

	return display
}

// withoutStatus drops the " (available)" style suffix that records whether attributes were readable.
func withoutStatus(value string) string {
	if !strings.HasSuffix(value, ")") {
		return value
	}

	if before, _, found := strings.CutLast(value, statusOpening); found {
		return before
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
	case fieldType:
		return displayType(field.Before), displayType(field.After)
	case fieldHash:
		return displayHash(field.Before), displayHash(field.After)
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

	render := func(value timeValue, precise bool) string {
		switch {
		case value.nanoseconds == 0:
			return humanize.Absent
		case !value.known:
			return unknownValue
		case precise:
			return humanize.PreciseTime(value.nanoseconds)
		default:
			return humanize.Time(value.nanoseconds)
		}
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
		shownBefore += " (" + before + " B)"
		shownAfter += " (" + after + " B)"
	}

	return shownBefore, shownAfter
}

func displayMode(value string) string {
	mode, err := strconv.ParseUint(value, 10, 32)
	// Recorded modes are complete st_mode values; anything without file type bits is shown as is.
	if err != nil || mode&fileTypeMask == 0 {
		return value
	}

	return humanize.Mode(uint32(mode))
}

func displayType(value string) string {
	var kind string

	if json.Unmarshal([]byte(value), &kind) != nil {
		return value
	}

	if kind == "" {
		return humanize.Absent
	}

	return humanize.Printable(kind)
}

func displayHash(value string) string {
	if value == "" || value == absentHash {
		return humanize.Absent
	}

	return value
}
