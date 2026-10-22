package integrity_test

import (
	"slices"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
)

const (
	fieldBirth = 1_000_000_000_000_000_000
	userXattr  = "user.a"
)

func fieldRecord() integrity.Record {
	return integrity.Record{
		Type: integrity.TypeRegular, Algorithm: hashSHA256, Hash: "a", Target: []byte("a"),
		Atime: fieldBirth, Mtime: fieldBirth, Btime: fieldBirth, HasBtime: true,
		AttributesStatus: integrity.AttributesAvailable,
		Xattrs:           []integrity.Attribute{{Name: []byte(userXattr), Value: []byte("1")}},
		ACL:              []integrity.Attribute{{Name: []byte("system.posix_acl_access"), Value: []byte("1")}},
	}
}

// Each compared field is a change on its own; recorded evidence outside that policy never is.
func TestOnlyComparedFieldsAreChanges(t *testing.T) {
	t.Parallel()

	first := fieldRecord()

	for field, change := range map[string]func(*integrity.Record){
		"hash":   func(record *integrity.Record) { record.Hash = "b" },
		"type":   func(record *integrity.Record) { record.Type = integrity.TypeSymlink },
		"target": func(record *integrity.Record) { record.Target = []byte("b") },
		"mode":   func(record *integrity.Record) { record.Mode = 1 },
		"uid":    func(record *integrity.Record) { record.UID = 1 },
		"gid":    func(record *integrity.Record) { record.GID = 1 },
	} {
		second := first
		change(&second)

		if got := integrity.Differences(first, second, config.Integrity{}); !slices.Equal(got, []string{field}) {
			t.Errorf("%s change: got %q", field, got)
		}
	}

	second := first
	second.Size, second.Inode, second.Device, second.Nlink = 1, 1, 1, 1
	second.Mtime, second.Ctime, second.Atime = 2*fieldBirth, 2*fieldBirth, 2*fieldBirth
	second.Xattrs, second.ACL = nil, nil

	if got := integrity.Differences(first, second, config.Integrity{}); len(got) != 0 {
		t.Fatalf("evidence outside the policy made a change: %q", got)
	}

	if integrity.SameVersion(first, second) {
		t.Fatal("new size, times and inode still reuse the recorded digest")
	}
}

// Extended attributes and ACLs are changes only when selected, and only where both observations could
// read them.
func TestSelectedAttributesAreChanges(t *testing.T) {
	t.Parallel()

	first := fieldRecord()

	for _, fixture := range []struct {
		name        string
		change      func(*integrity.Record)
		want        []string
		xattrs, acl bool
	}{
		{"xattr not selected", func(record *integrity.Record) {
			record.Xattrs = []integrity.Attribute{{Name: []byte(userXattr), Value: []byte("2")}}
		}, nil, false, true},
		{"xattr selected", func(record *integrity.Record) {
			record.Xattrs = []integrity.Attribute{{Name: []byte("security.capability"), Value: []byte("1")}}
		}, []string{integrity.FieldXattrs}, true, true},
		{"acl not selected", func(record *integrity.Record) { record.ACL = nil }, nil, true, false},
		{"acl selected", func(record *integrity.Record) { record.ACL = nil }, []string{integrity.FieldACL}, true, true},
		{"unsupported side", func(record *integrity.Record) {
			record.AttributesStatus, record.Xattrs, record.ACL = integrity.AttributesUnsupported, nil, nil
		}, nil, true, true},
		{"unreadable attributes", func(record *integrity.Record) {
			record.AttributesStatus, record.Xattrs, record.ACL = integrity.AttributesTooLarge, nil, nil
		}, []string{integrity.FieldXattrs}, true, true},
		{"unreadable attributes, ACL only", func(record *integrity.Record) {
			record.AttributesStatus, record.Xattrs, record.ACL = integrity.AttributesTooLarge, nil, nil
		}, nil, false, true},
	} {
		second := first
		fixture.change(&second)

		var policy config.Integrity

		policy.Xattrs, policy.ACL = fixture.xattrs, fixture.acl

		if got := integrity.Differences(first, second, policy); !slices.Equal(got, fixture.want) {
			t.Errorf("%s: got %q, want %q", fixture.name, got, fixture.want)
		}
	}
}

// A modification time moved back before the birth of the same file is a change; a recreated copy that
// keeps an old time is not.
func TestBackdatedModificationTimeIsAChange(t *testing.T) {
	t.Parallel()

	first := fieldRecord()
	backdated := first.Btime - int64(2*time.Hour)

	for _, fixture := range []struct {
		name   string
		change func(*integrity.Record)
		want   []string
	}{
		{"touch -d", func(record *integrity.Record) { record.Mtime = backdated }, []string{integrity.FieldMtime}},
		{"recreated copy", func(record *integrity.Record) {
			record.Mtime, record.Btime = backdated, first.Btime+int64(time.Minute)
		}, nil},
		{"within the threshold", func(record *integrity.Record) {
			record.Mtime = first.Btime - int64(time.Minute)
		}, nil},
		{"unknown birth", func(record *integrity.Record) { record.Mtime, record.HasBtime = backdated, false }, nil},
	} {
		second := first
		fixture.change(&second)

		if got := integrity.Differences(first, second, config.Integrity{}); !slices.Equal(got, fixture.want) {
			t.Errorf("%s: got %q, want %q", fixture.name, got, fixture.want)
		}
	}

	// The same backdated time observed again is not a new change.
	stale := first
	stale.Mtime = backdated

	if got := integrity.Differences(stale, stale, config.Integrity{}); len(got) != 0 {
		t.Fatalf("unchanged backdated time made a change: %q", got)
	}

	unknown := stale
	unknown.HasBtime = false

	if stale.Backdated() != 2*time.Hour || first.Backdated() != 0 || unknown.Backdated() != 0 {
		t.Fatal("Backdated misreports the gap")
	}
}
