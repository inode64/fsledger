package catalog

import "github.com/inode64/fsledger/internal/integrity"

const (
	fieldType             = integrity.FieldType
	fieldHash             = integrity.FieldHash
	fieldSize             = "size"
	fieldInode            = "inode"
	fieldDevice           = "device"
	fieldMode             = integrity.FieldMode
	fieldUID              = integrity.FieldUID
	fieldGID              = integrity.FieldGID
	fieldNlink            = "nlink"
	fieldMtime            = integrity.FieldMtime
	fieldCtime            = "ctime"
	fieldAtime            = "atime"
	fieldBtime            = "btime"
	fieldHasBtime         = "has_btime"
	fieldTarget           = integrity.FieldTarget
	fieldXattrs           = integrity.FieldXattrs
	fieldACL              = integrity.FieldACL
	fieldAttributesStatus = "attributes_status"
)

// VisibleFields returns the fields for human-readable templates, leaving out inode numbers; stored
// evidence remains unchanged.
func (detail Detail) VisibleFields() []FieldChange {
	var visible []FieldChange

	for _, field := range detail.Fields {
		if field.Field != fieldInode {
			visible = append(visible, field)
		}
	}

	return visible
}

// Code classifies observed changes. A missing content comparison must never be presented as proof of
// a content change.
func (detail Detail) Code() string {
	switch detail.Kind {
	case changeAdded:
		return "C"
	case changeDeleted:
		return "D"
	}

	classes := classifyFields(detail.Fields)
	switch {
	case classes.content && classes.permissions:
		return "MP"
	case classes.content:
		return "M"
	case classes.permissions:
		return "P"
	case classes.retimed && detail.BackdatedBy > 0:
		return "T"
	default:
		return "—"
	}
}

type changeClasses struct{ content, permissions, retimed bool }

func classifyFields(fields []FieldChange) changeClasses {
	var classes changeClasses

	for _, field := range fields {
		switch field.Field {
		case fieldHash, fieldTarget, fieldType:
			classes.content = true
		case fieldMode, fieldUID, fieldGID, fieldXattrs, fieldACL:
			classes.permissions = true
		case fieldMtime:
			// A backdated time classifies only on its own, and only on a backdated version: details
			// stored by an earlier policy carry mtime for any time change.
			classes.retimed = true
		}
	}

	return classes
}
