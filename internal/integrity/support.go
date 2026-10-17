package integrity

import "golang.org/x/sys/unix"

// Attribute status values record whether xattrs and ACLs could be part of an observation.
const (
	AttributesAvailable   = "available"
	AttributesUnsupported = "unsupported"
	// AttributesNotRecorded marks imported references whose source never recorded attributes.
	AttributesNotRecorded = "not_recorded_by_aide"
)

// AttributesSupported reports whether xattrs and ACLs of a record are evidence. Unsupported or
// unrecorded attributes are neither shown nor compared; an empty status belongs to an absent side.
func AttributesSupported(status string) bool {
	return status != AttributesUnsupported && status != AttributesNotRecorded
}

// atimeMaintained reports whether the kernel keeps access times current on a mount: noatime freezes
// them, relatime updates them at most once a day, and nodiratime freezes those of directories.
func atimeMaintained(flags int64, directory bool) bool {
	if flags&(unix.ST_NOATIME|unix.ST_RELATIME) != 0 {
		return false
	}

	return !directory || flags&unix.ST_NODIRATIME == 0
}
