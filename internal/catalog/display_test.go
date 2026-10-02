package catalog_test

import (
	"reflect"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
)

const displayACL = "acl"

func displayDetail(kind string, fields ...catalog.FieldChange) catalog.Detail {
	return catalog.Detail{Kind: kind, Fields: fields, ChangeID: "", Actor: "", Path: nil, Baseline: nil, Observed: 0}
}

func TestDisplayCodesUseObservedDifferences(t *testing.T) {
	t.Parallel()

	changed := func(field string) catalog.FieldChange {
		return catalog.FieldChange{Field: field, Before: "before", After: "after"}
	}
	for _, fixture := range []struct {
		name, want string
		detail     catalog.Detail
	}{
		{"creation", "C", displayDetail("added", changed("mode"))},
		{"deletion", "D", displayDetail("deleted", changed("hash"))},
		{"content", "M", displayDetail("modified", changed("hash"), changed("mtime"))},
		{"size", "M", displayDetail("modified", changed("size"))},
		{"symlink", "M", displayDetail("modified", changed("target"))},
		{"permissions", "P", displayDetail("modified", changed("mode"), changed("ctime"))},
		{"owner", "P", displayDetail("modified", changed("uid"), changed("gid"))},
		{"acl-values", "P", displayDetail("modified",
			catalog.FieldChange{Field: displayACL, Before: "old-acl (available)", After: "new-acl (available)"})},
		{"acl-unreadable", "—", displayDetail("modified",
			catalog.FieldChange{Field: displayACL, Before: "[] (available)", After: "[] (denied)"})},
		{"both", "MP", displayDetail("modified", changed("hash"), changed("mode"))},
		{"timestamps", "—", displayDetail("modified", changed("mtime"), changed("ctime"))},
		{"inode", "—", displayDetail("modified", changed("inode"))},
		{"other-metadata", "—", displayDetail("modified", changed("mode"), changed("xattrs"))},
		{"no-evidence", "—", displayDetail("modified")},
		{"equal-hash", "P", displayDetail("modified", changed("mode"),
			catalog.FieldChange{Field: "hash", Before: "same", After: "same"})},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			// A historical content violation must not turn today's chmod into MP.
			fixture.detail.Baseline = []catalog.FieldChange{changed("hash")}
			if code := fixture.detail.Code(); code != fixture.want {
				t.Fatalf("got %q, want %q", code, fixture.want)
			}
		})
	}
}

func TestVisibleDifferencesPreserveStoredEvidence(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"added", "modified", "deleted"} {
		detail := displayDetail(kind,
			catalog.FieldChange{Field: "inode", Before: "123", After: "456"},
			catalog.FieldChange{Field: "uid", Before: "0", After: "0"},
			catalog.FieldChange{Field: "mode", Before: "600", After: "644"},
		)
		detail.Baseline = detail.Fields

		want := []catalog.FieldChange{detail.Fields[2]}
		if !reflect.DeepEqual(detail.VisibleFields(), want) || !reflect.DeepEqual(detail.VisibleBaseline(), want) {
			t.Fatal("unchanged values or inode leaked", kind, detail.VisibleFields(), detail.VisibleBaseline())
		}

		visible := detail.VisibleFields()
		if len(visible) != 1 {
			t.Fatal("expected one visible difference", visible)
		}

		visible[0].Before = "mutated"
		if len(detail.Fields) != 3 || detail.Fields[2].Before != "600" {
			t.Fatal("stored evidence mutated")
		}
	}
}
