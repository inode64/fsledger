// Package integrity captures filesystem metadata and compares consecutive observations.
package integrity

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"fmt"
	"hash"
	"slices"
	"time"

	"github.com/inode64/fsledger/internal/config"

	"github.com/inode64/fsledger/internal/fault"

	"github.com/cespare/xxhash/v2"
	"github.com/zeebo/blake3"
)

// Attribute preserves arbitrary Linux names and values without UTF-8 conversion.
type Attribute struct {
	Name  []byte `json:"name"`
	Value []byte `json:"value"`
}

// Record is one stable observation. Times are Unix nanoseconds, not creation aliases.
type Record struct {
	Type             string      `json:"type"`
	AttributesStatus string      `json:"attributes_status"`
	Algorithm        string      `json:"algorithm,omitempty"`
	Hash             string      `json:"hash,omitempty"`
	Target           []byte      `json:"target,omitempty"`
	ACL              []Attribute `json:"acl"`
	Xattrs           []Attribute `json:"xattrs"`
	Path             []byte      `json:"path"`
	Size             int64       `json:"size"`
	Inode            uint64      `json:"inode"`
	Mtime            int64       `json:"mtime"`
	Ctime            int64       `json:"ctime"`
	Btime            int64       `json:"btime"`
	HashedAt         int64       `json:"hashed_at"`
	Observed         int64       `json:"observed"`
	Nlink            uint64      `json:"nlink"`
	Atime            int64       `json:"atime"`
	Device           uint64      `json:"device"`
	GID              uint32      `json:"gid"`
	UID              uint32      `json:"uid"`
	Mode             uint32      `json:"mode"`
	NoAtime          bool        `json:"no_atime"`
	HasBtime         bool        `json:"has_btime"`
}

// Hasher selects an explicit algorithm.
func Hasher(algorithm string) (hash.Hash, error) {
	switch algorithm {
	case config.HashSHA256:
		return sha256.New(), nil
	case config.HashSHA512:
		return sha512.New(), nil
	case config.HashSHA512256:
		return sha512.New512_256(), nil
	case config.HashSHA3256:
		return sha3.New256(), nil
	case config.HashSHA3512:
		return sha3.New512(), nil
	case config.HashBLAKE3:
		return blake3.New(), nil
	case config.HashXXHash64:
		return xxhash.New(), nil
	default:
		return nil, fault.New(fmt.Sprintf("unsupported hash algorithm %q", algorithm))
	}
}

// Names of the compared fields, as Differences reports them.
const (
	FieldHash   = "hash"
	FieldType   = "type"
	FieldTarget = "target"
	FieldMode   = "mode"
	FieldUID    = "uid"
	FieldGID    = "gid"
	FieldXattrs = "xattrs"
	FieldACL    = "acl"
	FieldMtime  = "mtime"
)

// backdatedThreshold ignores clock granularity and short copy delays; real backdating spans far more.
const backdatedThreshold = time.Hour

// Differences returns the fields that changed between two observations of a path, in this order:
// hash, type, target, mode, uid and gid, that is content, file type, link target, permissions and
// ownership; xattrs and acl when the policy selects them; and mtime when it moved back before the
// birth of the same file. Sizes, other times, links and inode numbers are recorded as evidence but
// never make a change on their own.
func Differences(previous, current Record, policy config.Integrity) []string {
	var changed []string

	if previous.Hash != current.Hash || previous.Algorithm != current.Algorithm {
		changed = append(changed, FieldHash)
	}

	if previous.Type != current.Type {
		changed = append(changed, FieldType)
	}

	if !bytes.Equal(previous.Target, current.Target) {
		changed = append(changed, FieldTarget)
	}

	if previous.Mode != current.Mode {
		changed = append(changed, FieldMode)
	}

	if previous.UID != current.UID {
		changed = append(changed, FieldUID)
	}

	if previous.GID != current.GID {
		changed = append(changed, FieldGID)
	}

	if policy.Xattrs &&
		attributesDiffer(previous.AttributesStatus, current.AttributesStatus, previous.Xattrs, current.Xattrs) {
		changed = append(changed, FieldXattrs)
	}

	if policy.ACL && previous.AttributesStatus == AttributesAvailable &&
		current.AttributesStatus == AttributesAvailable && !AttributesEqual(previous.ACL, current.ACL) {
		changed = append(changed, FieldACL)
	}

	if retimed(previous, current) {
		changed = append(changed, FieldMtime)
	}

	return changed
}

// attributesDiffer compares xattrs only when both observations could read them: a filesystem without
// xattrs is no evidence that they were removed, but xattrs growing past the observation limit are a
// change of their own. ACLs compare only where both lists were read, since losing the ability to read
// them is no evidence that permissions changed.
func attributesDiffer(beforeStatus, afterStatus string, before, after []Attribute) bool {
	return AttributesSupported(beforeStatus) && AttributesSupported(afterStatus) &&
		(beforeStatus != afterStatus || !AttributesEqual(before, after))
}

// AttributesEqual compares raw attribute names and values in their recorded order.
func AttributesEqual(first, second []Attribute) bool {
	return slices.EqualFunc(first, second, func(a, b Attribute) bool {
		return bytes.Equal(a.Name, b.Name) && bytes.Equal(a.Value, b.Value)
	})
}

// retimed reports a modification time moved back before the birth of the same file, as touch -d or
// utimensat leave it. A recreated file has a new birth time, so copies that keep times (tar, rsync -a,
// unzip) only count when their content differs too.
func retimed(previous, current Record) bool {
	return previous.HasBtime && previous.Btime == current.Btime && previous.Mtime != current.Mtime &&
		current.Backdated() > 0
}

// Backdated returns how much earlier the modification time of this version is than its birth time, or
// zero below backdatedThreshold. Copies that keep times (tar, rsync -a, cp -p) produce it as well as a
// backdated touch, so it is a hint rather than a verdict. Unknown times give zero.
func (record Record) Backdated() time.Duration {
	if !record.HasBtime || record.Mtime == 0 || record.Btime == 0 {
		return 0
	}

	if gap := time.Duration(record.Btime - record.Mtime); gap > backdatedThreshold {
		return gap
	}

	return 0
}

// SameVersion reports whether stat metadata shows the same version of a file, so the digest recorded
// with previous still describes current without reading its content again.
func SameVersion(previous, current Record) bool {
	return previous.Type == current.Type && previous.Size == current.Size &&
		previous.Mtime == current.Mtime && previous.Ctime == current.Ctime &&
		previous.Inode == current.Inode && previous.Device == current.Device
}
