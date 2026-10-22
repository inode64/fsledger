package catalog

import (
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

// Attributes too large to observe are a failed read, reported once, unlike attributes a filesystem
// does not provide.
func TestOversizedAttributesDisplayAsFailedRead(t *testing.T) {
	t.Parallel()

	oversized := integrity.Record{
		Path: []byte("/home/user/oversized"), Type: integrity.TypeRegular, Mtime: 1, Ctime: 1,
		AttributesStatus: integrity.AttributesTooLarge,
	}

	fields, err := describeFields(integrity.Record{}, oversized, []string{changeAdded})
	if err != nil {
		t.Fatal(err)
	}

	detail := Detail{
		ChangeID:    "",
		Kind:        changeAdded,
		Actor:       "",
		Path:        oversized.Path,
		Fields:      fields,
		Observed:    0,
		BackdatedBy: 0,
	}
	want := FieldChange{Field: fieldAttributesStatus, Before: "—", After: integrity.AttributesTooLarge}

	var reported []FieldChange

	for _, field := range detail.DisplayFields() {
		if field.Field == fieldAttributesStatus || field.Field == fieldACL {
			reported = append(reported, field)
		}
	}

	if len(reported) != 1 || reported[0] != want {
		t.Errorf("attributes shown as %q, want %q", reported, want)
	}
}
