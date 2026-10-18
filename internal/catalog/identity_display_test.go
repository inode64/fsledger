package catalog_test

import (
	"os/user"
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/humanize"
)

const (
	identityUID     = "uid"
	identityGID     = "gid"
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
		{identityUID, account.Uid, account.Username},
		{identityGID, group.Gid, group.Name},
	} {
		raw := displayField(fixture.field, "4294967295", fixture.id)
		want := displayField(fixture.field, raw.Before, fixture.id+" ("+humanize.Printable(fixture.name)+")")
		detail := displayDetail("modified", raw)
		detail.Baseline = []catalog.FieldChange{raw}

		if got := detail.DisplayFields(); !slices.Equal(got, []catalog.FieldChange{want}) {
			t.Fatalf("ownership display = %v, want %v", got, want)
		}

		if got := detail.DisplayBaseline(); !slices.Equal(got, []catalog.FieldChange{want}) {
			t.Fatalf("baseline display = %v, want %v", got, want)
		}

		if detail.Fields[0] != raw || detail.Baseline[0] != raw || detail.Code() != "P" {
			t.Fatal("display changed the evidence or classification", detail)
		}
	}
}

func TestDisplayOwnershipMissingAndAbsentValues(t *testing.T) {
	t.Parallel()

	for _, field := range []string{identityUID, identityGID} {
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
