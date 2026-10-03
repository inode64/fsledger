package aide

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
)

// Attribute positions are the public AIDE DB bitmask, not the column order.
// See AIDE include/attributes.h and src/db.c (0.19.3).
const attributeNames = "name lname perm uid gid size atime ctime mtime inode bcount lcount " +
	"md5 sha1 rmd160 tiger crc32 haval gost crc32b attr acl - - - - - - - - " +
	"sha256 sha512 selinux xattrs whirlpool - e2fsattrs capabilities stribog256 stribog512 - - " +
	"sha512_256 sha3_256 sha3_512 fstype"

const (
	columnName = "name"
	fieldPerm  = "perm"
	fieldInode = "inode"
	fieldLname = "lname"
	fieldUID   = "uid"
	fieldGID   = "gid"
	fieldAtime = "atime"
	fieldMtime = "mtime"
	fieldCtime = "ctime"
)

const transformedHashes = uint64(1<<40 | 1<<41) // growing and compressed hashing.

// Entry retains availability independently of zero-valued metadata.
type Entry struct {
	available   map[string]bool
	Record      integrity.Record
	transformed bool
}

// Validate requires every applicable configured comparison to have original evidence.
func (entry *Entry) Validate(policy config.Integrity) error {
	for _, field := range policy.Compare {
		if field == "hash" && entry.Record.Type != integrity.TypeRegular ||
			field == "target" && entry.Record.Type != integrity.TypeSymlink {
			continue
		}

		if !entry.available[field] {
			return fault.New(fmt.Sprintf(
				"AIDE path %q lacks configured field %q (hash algorithm %s); review integrity.compare/hash",
				entry.Record.Path,
				field,
				policy.Hash.Algorithm,
			))
		}

		if field == "hash" && entry.transformed {
			return fault.New("AIDE growing/compressed hashes cannot be used as full-file baselines")
		}
	}

	return nil
}

func decode(columns, values []string, algorithm string, hashSize int) (Entry, error) {
	if len(values) > len(columns) && strings.HasPrefix(values[len(columns)], "#") {
		values = values[:len(columns)]
	}

	if len(values) != len(columns) {
		return Entry{}, fault.New("AIDE row has the wrong number of columns")
	}

	fields := make(map[string]string, len(columns))

	var mask uint64

	for index, column := range columns {
		fields[column] = values[index]
	}

	if encoded, explicit := fields["attr"]; explicit {
		value, err := strconv.ParseUint(encoded, 10, 64)
		if err != nil {
			return Entry{}, fault.Wrap("invalid AIDE attribute mask", err)
		}

		mask = value
	} else {
		for _, column := range columns {
			mask |= attributeBit(column)
		}
	}

	entry := Entry{
		available:   make(map[string]bool),
		Record:      integrity.Record{AttributesStatus: "not_recorded_by_aide", TimesInSeconds: true},
		transformed: mask&transformedHashes != 0,
	}

	err := entry.decodeMetadata(fields, mask)
	if err != nil {
		return Entry{}, err
	}

	err = entry.decodeHash(fields, mask, algorithm, hashSize)
	if err != nil {
		return Entry{}, err
	}

	return entry, nil
}

func (entry *Entry) decodeMetadata(fields map[string]string, mask uint64) error {
	for column, value := range fields {
		field := fieldName(column)
		if field == "" {
			continue
		}

		// AIDE always writes the actual inode and mode, even when their comparison bits are unset.
		if column != columnName && column != fieldPerm && column != fieldInode && mask&attributeBit(column) == 0 {
			continue
		}

		err := setField(&entry.Record, column, value)
		if err != nil {
			return fmt.Errorf("AIDE field %s: %w", column, err)
		}

		entry.available[field] = true
	}

	entry.Record.Type = integrity.FileType(entry.Record.Mode)
	if entry.Record.Type == "" {
		return fault.New("invalid AIDE file type")
	}

	entry.available["type"] = true
	if entry.Record.Type == integrity.TypeSymlink && entry.available["target"] && entry.Record.Target == nil {
		return fault.New("missing AIDE symlink target")
	}

	return config.CleanAbsolute(string(entry.Record.Path))
}

func (entry *Entry) decodeHash(fields map[string]string, mask uint64, algorithm string, hashSize int) error {
	value, exists := fields[algorithm]
	if !exists || value == "0" || mask&attributeBit(algorithm) == 0 || entry.Record.Type != integrity.TypeRegular {
		return nil
	}

	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return fault.Wrap("invalid AIDE hash", err)
	}

	if len(decoded) != hashSize {
		return fault.New("invalid AIDE hash length")
	}

	entry.Record.Hash, entry.Record.Algorithm = hex.EncodeToString(decoded), algorithm
	entry.available["hash"] = true

	return nil
}

//nolint:gochecknoglobals // Immutable lookup derived once from the documented AIDE field order.
var attributeBits = func() map[string]uint64 {
	bits := make(map[string]uint64)

	for index, name := range strings.Fields(attributeNames) {
		if name != "-" {
			bits[name] = uint64(1) << index
		}
	}

	return bits
}()

func attributeBit(name string) uint64 { return attributeBits[name] }

func fieldName(column string) string {
	switch column {
	case columnName:
		return "path"
	case fieldLname:
		return "target"
	case fieldPerm:
		return "mode"
	case "lcount":
		return "nlink"
	case fieldUID, fieldGID, "size", fieldAtime, fieldMtime, fieldCtime, fieldInode:
		return column
	default:
		return ""
	}
}

func setField(record *integrity.Record, column, value string) error {
	switch column {
	case columnName, fieldLname:
		return setPath(record, column, value)
	case fieldMtime, fieldCtime, fieldAtime:
		return setTime(record, column, value)
	default:
		return setNumber(record, column, value)
	}
}

func setPath(record *integrity.Record, column, value string) error {
	if column == fieldLname {
		if value == "0" {
			return nil
		}

		if value == "0-" {
			record.Target = []byte{}

			return nil
		}

		if strings.HasPrefix(value, "00") {
			value = value[1:]
		}
	}

	decoded, err := url.PathUnescape(value)
	if err != nil {
		return fault.Wrap("invalid AIDE escaped path", err)
	}

	if strings.ContainsRune(decoded, 0) {
		return fault.New("NUL in AIDE path or link target")
	}

	if column == columnName {
		record.Path = []byte(decoded)
	} else {
		record.Target = []byte(decoded)
	}

	return nil
}

func setNumber(record *integrity.Record, column, value string) error {
	base, bits := 10, 64
	if column == fieldPerm {
		base = 8
	}

	if column == fieldPerm || column == fieldUID || column == fieldGID {
		bits = 32
	}

	number, err := strconv.ParseUint(value, base, bits)
	if err != nil {
		return fault.Wrap("invalid AIDE number", err)
	}

	switch column {
	case fieldPerm:
		//nolint:gosec // ParseUint uses a 32-bit limit for perm, uid and gid above.
		record.Mode = uint32(number)
	case fieldUID:
		//nolint:gosec // ParseUint uses a 32-bit limit for perm, uid and gid above.
		record.UID = uint32(number)
	case fieldGID:
		//nolint:gosec // ParseUint uses a 32-bit limit for perm, uid and gid above.
		record.GID = uint32(number)
	case "size":
		if number > math.MaxInt64 {
			return fault.New("AIDE file size overflows int64")
		}

		record.Size = int64(number)
	case fieldInode:
		record.Inode = number
	case "lcount":
		record.Nlink = number
	}

	return nil
}

func setTime(record *integrity.Record, column, value string) error {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		decoded, decodeErr := base64.StdEncoding.Strict().DecodeString(value)
		if decodeErr != nil {
			return fault.Wrap("invalid AIDE timestamp", decodeErr)
		}

		seconds, err = strconv.ParseInt(string(decoded), 10, 64)
	}

	if err != nil {
		return fault.Wrap("invalid AIDE timestamp", err)
	}

	if seconds > math.MaxInt64/int64(time.Second) || seconds < math.MinInt64/int64(time.Second) {
		return fault.New("AIDE timestamp overflows nanoseconds")
	}

	nanoseconds := seconds * int64(time.Second)

	switch column {
	case fieldMtime:
		record.Mtime = nanoseconds
	case fieldCtime:
		record.Ctime = nanoseconds
	case fieldAtime:
		record.Atime = nanoseconds
	}

	return nil
}
