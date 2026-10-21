package catalog

import (
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

// Attributes too large to observe are a failed read, reported once, unlike attributes a filesystem
// does not provide.
func TestOversizedAttributesDisplayAsFailedRead(t *testing.T) {
	t.Parallel()

	readable := integrity.Record{
		Path: []byte("/home/user/oversized"), Type: integrity.TypeRegular, Mtime: 1, Ctime: 1,
		AttributesStatus: integrity.AttributesAvailable,
		Xattrs:           []integrity.Attribute{{Name: []byte("user.a"), Value: []byte("1")}},
	}
	oversized := readable
	oversized.AttributesStatus, oversized.Xattrs = integrity.AttributesTooLarge, nil

	for _, fixture := range []struct {
		kind, before      string
		fields            []string
		previous, current integrity.Record
	}{
		{changeAdded, "—", []string{changeAdded}, integrity.Record{}, oversized},
		{
			changeModified, integrity.AttributesAvailable,
			integrity.Differences(readable, oversized, []string{fieldXattrs, fieldACL}), readable, oversized,
		},
	} {
		fields, err := describeFields(fixture.previous, fixture.current, fixture.fields)
		if err != nil {
			t.Fatal(err)
		}

		detail := Detail{
			ChangeID: "", Kind: fixture.kind, Actor: "", Path: oversized.Path,
			Fields: fields, Baseline: nil, Observed: 0,
		}
		want := FieldChange{Field: fieldAttributesStatus, Before: fixture.before, After: integrity.AttributesTooLarge}

		var reported []FieldChange

		for _, field := range detail.DisplayFields() {
			if field.Field == fieldAttributesStatus || field.Field == fieldACL {
				reported = append(reported, field)
			}
		}

		if len(reported) != 1 || reported[0] != want {
			t.Errorf("%s: attributes shown as %q, want %q", fixture.kind, reported, want)
		}
	}
}
