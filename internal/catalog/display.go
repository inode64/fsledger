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

	var content, permissions, retimed bool

	for _, field := range detail.Fields {
		switch field.Field {
		case fieldHash, fieldTarget, fieldType:
			content = true
		case fieldMode, fieldUID, fieldGID, fieldXattrs, fieldACL:
			permissions = true
		case fieldMtime:
			retimed = true
		}
	}

	// Differences reports mtime only when it moved back before btime; beside other changes it does not classify.
	switch {
	case content && permissions:
		return "MP"
	case content:
		return "M"
	case permissions:
		return "P"
	case retimed:
		return "T"
	default:
		return "—"
	}
}
