package integrity

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/config"
)

// POSIX ACLs are kept as these extended attributes.
const (
	aclAccess  = "system.posix_acl_access"
	aclDefault = "system.posix_acl_default"
)

// Attribute status values record whether xattrs and ACLs could be part of an observation.
const (
	AttributesAvailable   = "available"
	AttributesUnsupported = "unsupported"
	// AttributesTooLarge marks attributes beyond the observation limit: none are recorded, and the
	// record reports a failed read.
	AttributesTooLarge = "too_large"
)

// AttributesSupported reports whether xattrs and ACLs of a record are evidence. Unsupported attributes
// are not shown; an empty status belongs to an absent side.
func AttributesSupported(status string) bool {
	return status != AttributesUnsupported
}

// AttributeAdvisories names each attribute comparison policy selects that the filesystem of path, of
// type filesystem, does not support: those attributes are never compared there.
func AttributeAdvisories(policy config.Integrity, path, filesystem string) []string {
	if !policy.Xattrs && !policy.ACL {
		return nil
	}

	xattrs, acl := attributeSupport(path)

	return attributeAdvisories(policy, path, filesystem, xattrs, acl)
}

func attributeAdvisories(policy config.Integrity, path, filesystem string, xattrs, acl bool) []string {
	var advisories []string

	if policy.Xattrs && !xattrs {
		advisories = append(advisories, fmt.Sprintf("integrity.xattrs is selected but %s (%s) does not "+
			"support extended attributes; they are not compared", path, filesystem))
	}

	if policy.ACL && !acl {
		advisories = append(advisories, fmt.Sprintf("integrity.acl is selected but %s (%s) does not "+
			"support POSIX ACLs; they are not compared", path, filesystem))
	}

	return advisories
}

// attributeSupport reports whether the filesystem of path keeps extended attributes, then whether it
// keeps POSIX ACLs. Only the kernel's refusal counts: a path it cannot inspect, absent or unreadable,
// is not proof that the filesystem lacks them.
func attributeSupport(path string) (bool, bool) {
	// A symlink never carries ACLs, whatever its filesystem: probe the directory that holds it.
	var stat unix.Stat_t
	if unix.Lstat(path, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFLNK {
		path = filepath.Dir(path)
	}

	_, err := unix.Llistxattr(path, nil)
	if errors.Is(err, unix.ENOTSUP) {
		return false, false
	}

	// ENODATA, an entry without an ACL, means the filesystem keeps them.
	_, err = unix.Lgetxattr(path, aclAccess, nil)

	return true, !errors.Is(err, unix.ENOTSUP)
}

// atimeMaintained reports whether the kernel keeps access times current on a mount: noatime freezes
// them, relatime updates them at most once a day, and nodiratime freezes those of directories.
func atimeMaintained(flags int64, directory bool) bool {
	if flags&(unix.ST_NOATIME|unix.ST_RELATIME) != 0 {
		return false
	}

	return !directory || flags&unix.ST_NODIRATIME == 0
}
