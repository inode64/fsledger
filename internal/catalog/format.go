package catalog

import (
	"strconv"

	"github.com/inode64/fsledger/internal/integrity"
)

// printableField encodes only requested evidence, preserving JSON's byte and string
// escaping without serializing the entire record and decoding it into two maps.
//
//nolint:cyclop,funlen,gocyclo // Metadata fields form a flat formatting dispatch table.
func printableField(record integrity.Record, field string) (string, error) {
	var value any

	suffix := ""

	switch field {
	case fieldHash:
		return record.Algorithm + ":" + record.Hash, nil
	case fieldType:
		value = record.Type
	case fieldSize:
		value = record.Size
	case fieldInode:
		value = record.Inode
	case fieldDevice:
		value = record.Device
	case fieldMode:
		value = record.Mode
	case fieldUID:
		value = record.UID
	case fieldGID:
		value = record.GID
	case fieldNlink:
		value = record.Nlink
	case fieldMtime:
		value = record.Mtime
	case fieldCtime:
		value = record.Ctime
	case fieldAtime:
		value = record.Atime
	case fieldBtime:
		value = record.Btime
		suffix = " (known=" + strconv.FormatBool(record.HasBtime) + ")"
	case fieldHasBtime:
		value = record.HasBtime
	case fieldTarget:
		if len(record.Target) == 0 {
			return "", nil
		}

		value = record.Target
	case fieldXattrs:
		value = record.Xattrs
		suffix = " (" + record.AttributesStatus + ")"
	case fieldACL:
		value = record.ACL
		suffix = " (" + record.AttributesStatus + ")"
	case fieldAttributesStatus:
		value = record.AttributesStatus
	default:
		return "", nil
	}

	data, err := encode(value)

	return string(data) + suffix, err
}
