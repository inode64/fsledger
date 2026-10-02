package catalog

import "strings"

const (
	fieldType             = "type"
	fieldHash             = "hash"
	fieldSize             = "size"
	fieldInode            = "inode"
	fieldDevice           = "device"
	fieldMode             = "mode"
	fieldUID              = "uid"
	fieldGID              = "gid"
	fieldNlink            = "nlink"
	fieldMtime            = "mtime"
	fieldCtime            = "ctime"
	fieldAtime            = "atime"
	fieldBtime            = "btime"
	fieldHasBtime         = "has_btime"
	fieldTarget           = "target"
	fieldXattrs           = "xattrs"
	fieldACL              = "acl"
	fieldAttributesStatus = "attributes_status"
)

// VisibleFields returns only actual, non-inode differences for human-readable templates.
// Stored evidence remains unchanged, including for messages queued before this renderer existed.
func (detail Detail) VisibleFields() []FieldChange { return visibleFields(detail.Fields) }

// VisibleBaseline filters the separate approved-reference comparison in the same way.
func (detail Detail) VisibleBaseline() []FieldChange { return visibleFields(detail.Baseline) }

func visibleFields(fields []FieldChange) []FieldChange {
	var visible []FieldChange

	for _, field := range fields {
		if field.Field != fieldInode && field.Before != field.After {
			visible = append(visible, field)
		}
	}

	return visible
}

// Code classifies observed changes, not historical baseline differences. A missing
// content comparison must never be presented as proof of a content change.
func (detail Detail) Code() string {
	switch detail.Kind {
	case changeAdded:
		return "C"
	case changeDeleted:
		return "D"
	}

	classes := classifyFields(detail.VisibleFields())
	switch {
	case classes.content && classes.permissions:
		return "MP"
	case classes.content:
		return "M"
	case classes.permissions && !classes.other:
		return "P"
	default:
		return "—"
	}
}

type changeClasses struct{ content, permissions, other bool }

func classifyFields(fields []FieldChange) changeClasses {
	var classes changeClasses

	for _, field := range fields {
		switch field.Field {
		case fieldHash, fieldSize, fieldTarget, fieldType:
			classes.content = true
		case fieldMode, fieldUID, fieldGID:
			classes.permissions = true
		case fieldACL:
			if changedACL(field) {
				classes.permissions = true
			} else {
				classes.other = true
			}
		case fieldAtime, fieldMtime, fieldCtime, fieldBtime, fieldHasBtime:
			// Timestamps can accompany chmod without changing file content.
		default:
			classes.other = true
		}
	}

	return classes
}

func changedACL(field FieldChange) bool {
	// Losing the ability to read ACLs is not evidence that permissions changed.
	before, beforeKnown := strings.CutSuffix(field.Before, " (available)")
	after, afterKnown := strings.CutSuffix(field.After, " (available)")

	return beforeKnown && afterKnown && before != after
}
