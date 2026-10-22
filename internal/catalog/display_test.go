package catalog_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
)

const displayACL = "acl"

func displayDetail(kind string, fields ...catalog.FieldChange) catalog.Detail {
	return catalog.Detail{Kind: kind, Fields: fields, ChangeID: "", Actor: "", Path: nil, Observed: 0, BackdatedBy: 0}
}

func TestDisplayCodesUseObservedDifferences(t *testing.T) {
	t.Parallel()

	changed := func(field string) catalog.FieldChange {
		return catalog.FieldChange{Field: field, Before: "before", After: "after"}
	}
	// Only a backdated version makes an mtime change T; an earlier policy stored mtime for any time change.
	backdated := displayDetail("modified", changed("mtime"))
	backdated.BackdatedBy = time.Hour + 1

	for _, fixture := range []struct {
		name, want string
		detail     catalog.Detail
	}{
		{"creation", "C", displayDetail("added", changed("mode"))},
		{"deletion", "D", displayDetail("deleted", changed("hash"))},
		{"content", "M", displayDetail("modified", changed("hash"))},
		{"type", "M", displayDetail("modified", changed("type"))},
		{"symlink", "M", displayDetail("modified", changed("target"))},
		{"permissions", "P", displayDetail("modified", changed("mode"))},
		{"owner", "P", displayDetail("modified", changed("uid"), changed("gid"))},
		{"both", "MP", displayDetail("modified", changed("hash"), changed("mode"))},
		{"xattrs", "P", displayDetail("modified", changed("xattrs"))},
		{"acl", "P", displayDetail("modified", changed(displayACL))},
		{"backdated", "T", backdated},
		{"earlier-policy-time", "—", displayDetail("modified", changed("mtime"))},
		{"backdated-content", "M", displayDetail("modified", changed("hash"), changed("mtime"))},
		{"no-evidence", "—", displayDetail("modified")},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			if code := fixture.detail.Code(); code != fixture.want {
				t.Fatalf("got %q, want %q", code, fixture.want)
			}
		})
	}
}

func TestVisibleFieldsHideInodeWithoutMutatingEvidence(t *testing.T) {
	t.Parallel()

	detail := displayDetail("added",
		catalog.FieldChange{Field: "inode", Before: "123", After: "456"},
		catalog.FieldChange{Field: "uid", Before: "0", After: "0"},
		catalog.FieldChange{Field: "mode", Before: "600", After: "644"},
	)

	visible := detail.VisibleFields()
	if len(visible) != 2 || !reflect.DeepEqual(visible, detail.Fields[1:]) {
		t.Fatal("inode leaked or evidence hidden", visible)
	}

	visible[0].Before = "mutated"

	if detail.Fields[1].Before != "0" {
		t.Fatal("stored evidence mutated")
	}
}
