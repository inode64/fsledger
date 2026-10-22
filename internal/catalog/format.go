package catalog

import (
	"strconv"

	"github.com/inode64/fsledger/internal/integrity"
)

// Markers of stored field values. Display parses values back with the same markers, so formatting
// and parsing cannot drift apart.
const (
	hashSeparator = ":"
	suffixOpening = " ("
	suffixClosing = ")"
	knownLabel    = "known="
	knownTrue     = suffixOpening + knownLabel + "true" + suffixClosing
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
		return record.Algorithm + hashSeparator + record.Hash, nil
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
		suffix = suffixOpening + knownLabel + strconv.FormatBool(record.HasBtime) + suffixClosing
	case fieldHasBtime:
		value = record.HasBtime
	case fieldTarget:
		if len(record.Target) == 0 {
			return "", nil
		}

		value = record.Target
	case fieldXattrs:
		value = record.Xattrs
		suffix = suffixOpening + record.AttributesStatus + suffixClosing
	case fieldACL:
		value = record.ACL
		suffix = suffixOpening + record.AttributesStatus + suffixClosing
	case fieldAttributesStatus:
		value = record.AttributesStatus
	default:
		return "", nil
	}

	data, err := encode(value)

	return string(data) + suffix, err
}
