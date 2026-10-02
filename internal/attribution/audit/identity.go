package audit

import (
	"errors"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
)

func fileIdentity(file map[string]string) (uint64, uint64) {
	inode, err := strconv.ParseUint(file["inode"], 10, 64)
	if err != nil {
		return 0, 0
	}

	majorText, minorText, found := strings.Cut(file["dev"], ":")
	if !found {
		return 0, 0
	}

	major, majorErr := strconv.ParseUint(majorText, 16, 32)

	minor, minorErr := strconv.ParseUint(minorText, 16, 32)
	if majorErr != nil || minorErr != nil {
		return 0, 0
	}

	return unix.Mkdev(uint32(major), uint32(minor)), inode
}

func objectMatches(value observation) bool {
	if value.inode == 0 {
		return false
	}

	var stat unix.Stat_t

	err := unix.Lstat(value.path, &stat)
	if value.operation == event.Remove {
		return errors.Is(err, unix.ENOENT)
	}

	return err == nil && stat.Ino == value.inode && stat.Dev == value.device
}
