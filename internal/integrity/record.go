// Package integrity captures filesystem metadata and compares approved observations.
package integrity

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"strings"
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
	TimesInSeconds   bool        `json:"times_in_seconds,omitempty"`
}

// Hasher selects an explicit algorithm; callers must not use noncrypto hashes for baselines.
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

// Differences returns selected fields that changed, in policy order.
//
//nolint:cyclop,gocyclo // Explicit field comparisons form a flat policy dispatch table.
func Differences(previous, current Record, fields []string) []string {
	var changed []string

	for _, field := range fields {
		different := false

		switch field {
		case "hash":
			different = previous.Hash != current.Hash || previous.Algorithm != current.Algorithm
		case "type":
			different = previous.Type != current.Type
		case "mode":
			different = previous.Mode != current.Mode
		case "uid":
			different = previous.UID != current.UID
		case "gid":
			different = previous.GID != current.GID
		case "size":
			different = previous.Size != current.Size
		case "device":
			different = previous.Device != current.Device
		case "inode":
			different = previous.Inode != current.Inode
		case "nlink":
			different = previous.Nlink != current.Nlink
		case "atime":
			different = differentTime(previous.Atime, current.Atime, previous.TimesInSeconds || current.TimesInSeconds)
		case "mtime":
			different = differentTime(previous.Mtime, current.Mtime, previous.TimesInSeconds || current.TimesInSeconds)
		case "ctime":
			different = differentTime(previous.Ctime, current.Ctime, previous.TimesInSeconds || current.TimesInSeconds)
		case "btime":
			different = previous.Btime != current.Btime || previous.HasBtime != current.HasBtime
		case "target":
			different = !bytes.Equal(previous.Target, current.Target)
		case "xattrs":
			different = !AttributesEqual(previous.Xattrs, current.Xattrs) ||
				previous.AttributesStatus != current.AttributesStatus
		case "acl":
			different = !AttributesEqual(previous.ACL, current.ACL) ||
				previous.AttributesStatus != current.AttributesStatus
		}

		if different {
			changed = append(changed, field)
		}
	}

	return changed
}

func differentTime(first, second int64, seconds bool) bool {
	if seconds {
		return time.Unix(0, first).Unix() != time.Unix(0, second).Unix()
	}

	return first != second
}

// AttributesEqual compares raw attribute names and values in their recorded order.
func AttributesEqual(first, second []Attribute) bool {
	return slices.EqualFunc(
		first,
		second,
		func(a, b Attribute) bool { return bytes.Equal(a.Name, b.Name) && bytes.Equal(a.Value, b.Value) },
	)
}

// PolicyID prevents approving observations made under an obsolete comparison policy.
func PolicyID(fields []string, algorithm string) string {
	ordered := slices.Clone(fields)
	slices.Sort(ordered)
	sum := sha256.Sum256([]byte(strings.Join(ordered, "\x00") + "\x01" + algorithm))

	return hex.EncodeToString(sum[:])
}
