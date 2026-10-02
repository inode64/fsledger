package integrity

import "golang.org/x/sys/unix"

// File type names are shared by live observations, imported records and comparisons.
const (
	TypeRegular   = "regular"
	TypeDirectory = "directory"
	TypeSymlink   = "symlink"
	TypeFIFO      = "fifo"
	TypeSocket    = "socket"
	TypeCharacter = "character"
	TypeBlock     = "block"
	TypeSpecial   = "special"
)

// FileType translates Linux mode bits to a record type. Unknown modes return an
// empty string so importers can reject them; live observations may use TypeSpecial.
func FileType(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return TypeRegular
	case unix.S_IFDIR:
		return TypeDirectory
	case unix.S_IFLNK:
		return TypeSymlink
	case unix.S_IFIFO:
		return TypeFIFO
	case unix.S_IFSOCK:
		return TypeSocket
	case unix.S_IFCHR:
		return TypeCharacter
	case unix.S_IFBLK:
		return TypeBlock
	default:
		return ""
	}
}
