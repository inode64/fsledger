package integrity_test

import (
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

// A value only one side's filesystem provides is not evidence, so it never counts as a difference.
func TestUnsupportedAttributesAreNotCompared(t *testing.T) {
	t.Parallel()

	supported := integrity.Record{
		Atime: 1_000_000_000, Btime: 1_000_000_000, HasBtime: true,
		AttributesStatus: integrity.AttributesAvailable,
		ACL:              []integrity.Attribute{{Name: []byte("system.posix_acl_access"), Value: []byte("1")}},
	}

	for _, fixture := range []struct {
		name  string
		field string
		other integrity.Record
	}{
		{"atime-not-maintained", "atime", integrity.Record{Atime: 0}},
		{"btime-unknown", "btime", integrity.Record{HasBtime: false}},
		{"acl-unsupported", "acl", integrity.Record{AttributesStatus: integrity.AttributesUnsupported}},
		{"xattrs-not-recorded", "xattrs", integrity.Record{AttributesStatus: integrity.AttributesNotRecorded}},
	} {
		for _, pair := range [][2]integrity.Record{{supported, fixture.other}, {fixture.other, supported}} {
			if got := integrity.Differences(pair[0], pair[1], []string{fixture.field}); len(got) != 0 {
				t.Errorf("%s: unsupported side compared: %q", fixture.name, got)
			}
		}
	}

	changed := supported
	changed.Atime, changed.Btime, changed.ACL = 2_000_000_000, 2_000_000_000, nil

	got := integrity.Differences(supported, changed, []string{"atime", "btime", "acl"})
	if len(got) != 3 {
		t.Fatalf("supported attributes not compared: %q", got)
	}

	if !integrity.AttributesSupported("") || !integrity.AttributesSupported(integrity.AttributesAvailable) ||
		integrity.AttributesSupported(integrity.AttributesUnsupported) {
		t.Fatal("AttributesSupported misclassifies statuses")
	}
}
