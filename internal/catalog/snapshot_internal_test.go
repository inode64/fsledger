package catalog

import (
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

func snapshotNames(t *testing.T, previous, current integrity.Record, kind string) []string {
	t.Helper()

	fields, err := describeFields(previous, current, []string{kind})
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, len(fields))
	for index, field := range fields {
		names[index] = field.Field
	}

	return names
}

// Created and deleted entries record only attributes the filesystem of the existing version provides.
func TestSnapshotsLeaveOutUnsupportedAttributes(t *testing.T) {
	t.Parallel()

	unsupportedNames := []string{fieldAtime, fieldBtime, fieldHasBtime, fieldXattrs, fieldACL, fieldAttributesStatus}
	bare := integrity.Record{
		Path: []byte("/srv/www/index.php"), Type: integrity.TypeRegular, Mtime: 1, Ctime: 1,
		AttributesStatus: integrity.AttributesUnsupported,
	}
	full := bare
	full.Atime, full.Btime, full.HasBtime, full.AttributesStatus = 1, 1, true, integrity.AttributesAvailable

	for _, fixture := range []struct {
		name, kind        string
		previous, current integrity.Record
		want              bool
	}{
		{"created-without-support", changeAdded, integrity.Record{}, bare, false},
		{"deleted-without-support", changeDeleted, bare, integrity.Record{}, false},
		{"created-with-support", changeAdded, integrity.Record{}, full, true},
		{"deleted-with-support", changeDeleted, full, integrity.Record{}, true},
	} {
		names := snapshotNames(t, fixture.previous, fixture.current, fixture.kind)
		for _, name := range unsupportedNames {
			if slices.Contains(names, name) != fixture.want {
				t.Errorf("%s: %s present = %v, want %v (%q)", fixture.name, name, !fixture.want, fixture.want, names)
			}
		}

		if !slices.Contains(names, fieldMtime) || !slices.Contains(names, fieldCtime) {
			t.Errorf("%s: mtime or ctime missing: %q", fixture.name, names)
		}
	}
}
