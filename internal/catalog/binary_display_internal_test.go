package catalog

import (
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/humanize"
	"github.com/inode64/fsledger/internal/integrity"
)

const (
	binaryOldTarget = "/etc/old"
	binaryComment   = "user.comment"
	// binaryOwnerACL is system.posix_acl_access as the kernel stores it: version 2, then user::rw-.
	binaryOwnerACL = "\x02\x00\x00\x00\x01\x00\x06\x00\xff\xff\xff\xff"
)

// storedChange formats both versions as the catalog stores them: targets and attributes in base64.
func storedChange(t *testing.T, field string, previous, current integrity.Record) FieldChange {
	t.Helper()

	before, beforeErr := printableField(previous, field)
	after, afterErr := printableField(current, field)

	if beforeErr != nil || afterErr != nil {
		t.Fatal(beforeErr, afterErr)
	}

	return FieldChange{Field: field, Before: before, After: after}
}

func TestDisplayDecodesSymlinkTargets(t *testing.T) {
	t.Parallel()

	link := func(target string) integrity.Record {
		return integrity.Record{Type: integrity.TypeSymlink, Target: []byte(target)}
	}

	for _, fixture := range []struct {
		name, kind, before, after string
		previous, current         integrity.Record
	}{
		{"retargeted", changeModified, binaryOldTarget, "/tmp/evil", link(binaryOldTarget), link("/tmp/evil")},
		{"created", changeAdded, humanize.Absent, binaryOldTarget, integrity.Record{}, link(binaryOldTarget)},
		{
			"replaced-file", changeModified, humanize.Absent, binaryOldTarget,
			integrity.Record{Type: integrity.TypeRegular},
			link(binaryOldTarget),
		},
		// Targets are attacker-controlled: they must not add lines or hide characters.
		{
			"hostile", changeModified, binaryOldTarget, `/tmp/x\nRuta: "/etc/shadow"\r\x1b[2J\u202e\xff\\`,
			link(binaryOldTarget),
			link("/tmp/x\nRuta: \"/etc/shadow\"\r\x1b[2J\u202e\xff\\"),
		},
	} {
		want := []FieldChange{{Field: fieldTarget, Before: fixture.before, After: fixture.after}}
		if got := displayFields([]FieldChange{storedChange(t, fieldTarget, fixture.previous, fixture.current)},
			fixture.kind); !slices.Equal(got, want) {
			t.Errorf("%s: target = %q, want %q", fixture.name, got, want)
		}
	}
}

func TestDisplayDecodesAttributes(t *testing.T) {
	t.Parallel()

	// The same entries serve as xattrs and as ACLs; names and values alternate.
	attributes := func(status string, pairs ...string) integrity.Record {
		list := make([]integrity.Attribute, 0, len(pairs)/2)
		for index := 0; index+1 < len(pairs); index += 2 {
			list = append(list, integrity.Attribute{Name: []byte(pairs[index]), Value: []byte(pairs[index+1])})
		}

		return integrity.Record{AttributesStatus: status, Xattrs: list, ACL: list}
	}
	available := func(pairs ...string) integrity.Record { return attributes(integrity.AttributesAvailable, pairs...) }
	none := humanize.Absent + availableSuffix

	for _, fixture := range []struct {
		name, before, after string
		previous, current   integrity.Record
	}{
		{
			"text", binaryComment + `="old"` + availableSuffix,
			binaryComment + `="new", user.mime_type="text/plain"` + availableSuffix,
			available(binaryComment, "old"), available(binaryComment, "new", "user.mime_type", "text/plain"),
		},
		{
			"c-string", none, `security.selinux="system_u:object_r:etc_t:s0\x00"` + availableSuffix,
			available(), available("security.selinux", "system_u:object_r:etc_t:s0\x00"),
		},
		{
			"binary", none, "system.posix_acl_access=0x0200000001000600ffffffff" + availableSuffix,
			available(), available("system.posix_acl_access", binaryOwnerACL),
		},
		// Names and values are attacker-controlled: they must not add lines or pose as other entries.
		{
			"hostile", humanize.Absent + " (denied)",
			`user.\nRuta="x\", user.b=\"y", user.multiline=0x610a62` + availableSuffix,
			attributes("denied"), available("user.\nRuta", `x", user.b="y`, "user.multiline", "a\nb"),
		},
	} {
		for _, field := range []string{fieldXattrs, fieldACL} {
			want := []FieldChange{{Field: field, Before: fixture.before, After: fixture.after}}
			if got := displayFields([]FieldChange{storedChange(t, field, fixture.previous, fixture.current)},
				changeModified); !slices.Equal(got, want) {
				t.Errorf("%s %s = %q, want %q", fixture.name, field, got, want)
			}
		}
	}

	// Values that are not stored attribute lists are shown as they are.
	unparsed := FieldChange{Field: fieldACL, Before: "old-acl" + availableSuffix, After: "new-acl" + availableSuffix}
	if got := displayFields([]FieldChange{unparsed}, changeModified); !slices.Equal(got, []FieldChange{unparsed}) {
		t.Errorf("unparsed acl = %q", got)
	}
}
