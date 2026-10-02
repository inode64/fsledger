package integrity_test

import (
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
)

// Configuration accepts a field name only if Differences really compares it: an accepted but
// ignored name would silently disable that part of a policy.
func TestEveryConfigurableFieldIsCompared(t *testing.T) {
	t.Parallel()

	first := integrity.Record{
		Type: integrity.TypeRegular, Algorithm: hashSHA256, Hash: "a", Target: []byte("a"),
		Xattrs: []integrity.Attribute{{Name: []byte("user.a"), Value: []byte("1")}},
		ACL:    []integrity.Attribute{{Name: []byte("system.posix_acl_access"), Value: []byte("1")}},
	}
	second := integrity.Record{
		Type: integrity.TypeSymlink, Algorithm: hashSHA256, Hash: "b", Target: []byte("b"),
		Mode: 1, UID: 1, GID: 1, Size: 1, Inode: 1, Device: 1, Nlink: 1,
		Mtime: 2_000_000_000, Ctime: 2_000_000_000, Atime: 2_000_000_000, Btime: 2_000_000_000, HasBtime: true,
	}

	for _, field := range config.IntegrityFields {
		if !slices.Equal(integrity.Differences(first, second, []string{field}), []string{field}) {
			t.Errorf("integrity.compare accepts %q but Differences ignores it", field)
		}
	}
}
