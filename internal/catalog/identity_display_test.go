package catalog_test

import (
	"os/user"
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/humanize"
	"github.com/inode64/fsledger/internal/integrity"
)

const (
	identityAdded   = "added"
	identityDeleted = "deleted"
)

func TestDisplayOwnershipNamesPreserveIDsAndEvidence(t *testing.T) {
	t.Parallel()

	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		t.Fatal(err)
	}

	for _, fixture := range []struct{ field, id, name string }{
		{integrity.FieldUID, account.Uid, account.Username},
		{integrity.FieldGID, group.Gid, group.Name},
	} {
		raw := displayField(fixture.field, "4294967295", fixture.id)
		want := displayField(fixture.field, raw.Before, fixture.id+" ("+humanize.Printable(fixture.name)+")")
		detail := displayDetail("modified", raw)

		if got := detail.DisplayFields(); !slices.Equal(got, []catalog.FieldChange{want}) {
			t.Fatalf("ownership display = %v, want %v", got, want)
		}

		if detail.Fields[0] != raw || detail.Code() != "P" {
			t.Fatal("display changed the evidence or classification", detail)
		}
	}
}

func TestDisplayOwnershipMissingAndAbsentValues(t *testing.T) {
	t.Parallel()

	for _, field := range []string{integrity.FieldUID, integrity.FieldGID} {
		for _, kind := range []string{identityAdded, "modified", identityDeleted} {
			raw := displayField(field, "4294967295", "invalid")
			want := raw

			switch kind {
			case identityAdded:
				want.Before = humanize.Absent
			case identityDeleted:
				want.After = humanize.Absent
			}

			if got := displayDetail(kind, raw).DisplayFields(); !slices.Equal(got, []catalog.FieldChange{want}) {
				t.Fatalf("%s %s display = %v, want %v", field, kind, got, want)
			}
		}
	}
}

func TestDisplaySnapshotMarksAbsentSide(t *testing.T) {
	t.Parallel()

	created := []catalog.FieldChange{
		displayField(integrity.FieldType, `""`, `"regular"`),
		displayField(integrity.FieldHash, ":", "sha256:ea5c"),
		displayField(fieldNameSize, "0", "182422"),
		displayField(integrity.FieldUID, "0", "4294967295"),
		displayField(integrity.FieldMode, "0", "33212"),
		displayField(fieldNameDevice, "0", "64514"),
	}
	shown := []catalog.FieldChange{
		displayField(integrity.FieldType, humanize.Absent, "regular"),
		displayField(integrity.FieldHash, humanize.Absent, "sha256:ea5c"),
		displayField(fieldNameSize, humanize.Absent, "178.1 KiB"),
		displayField(integrity.FieldUID, humanize.Absent, "4294967295"),
		displayField(integrity.FieldMode, humanize.Absent, "-rw-rwxr-- (0x81bc)"),
	}

	for _, fixture := range []struct {
		kind           string
		snapshot, want []catalog.FieldChange
	}{
		{identityAdded, created, shown},
		{identityDeleted, swapSides(created), swapSides(shown)},
	} {
		if got := displayDetail(fixture.kind, fixture.snapshot...).DisplayFields(); !slices.Equal(got, fixture.want) {
			t.Errorf("%s snapshot display = %v, want %v", fixture.kind, got, fixture.want)
		}
	}
}

func swapSides(fields []catalog.FieldChange) []catalog.FieldChange {
	swapped := make([]catalog.FieldChange, 0, len(fields))
	for _, field := range fields {
		swapped = append(swapped, displayField(field.Field, field.After, field.Before))
	}

	return swapped
}
