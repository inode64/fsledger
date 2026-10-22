package catalog

import (
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/integrity"
)

const backdatedGap = 48 * time.Hour

// A touch -d on an existing file is a change of its own, classified T; any other change of a backdated
// version carries the gap, so readers see the hint.
func TestBackdatedVersionsAreChangesWithEvidence(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name, code                    string
		alreadyBefore, content, chmod bool
	}{
		{"touch -d", "T", false, false, false},
		{"content and time", "M", false, true, false},
		{"content of a backdated copy", "M", true, true, false},
		{"mode of a backdated copy", "P", true, false, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			original := workloadRecord(0)
			original.Btime, original.HasBtime = original.Mtime, true

			changed := original
			changed.Mtime -= int64(backdatedGap)
			changed.Ctime++

			if fixture.content {
				changed.Hash = workloadRecord(1).Hash
			}

			if fixture.chmod {
				changed.Mode = 0o755
			}

			if fixture.alreadyBefore {
				original.Mtime = changed.Mtime
			}

			detail := observeStagedChange(t, testStore(t), original, changed)

			if detail.Code() != fixture.code || detail.BackdatedBy != backdatedGap {
				t.Fatalf("code %q, backdated by %v: %+v", detail.Code(), detail.BackdatedBy, detail.Fields)
			}
		})
	}
}

// A selected attribute comparison makes a new file capability a permission change.
func TestSelectedXattrsChangeIsPermissions(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.policy.Xattrs = true

	original := workloadRecord(0)
	changed := original
	changed.Xattrs = []integrity.Attribute{{Name: []byte("security.capability"), Value: []byte{1}}}
	changed.Ctime++

	detail := observeStagedChange(t, store, original, changed)

	if detail.Code() != "P" || len(detail.Fields) != 1 || detail.Fields[0].Field != fieldXattrs {
		t.Fatalf("capability change: code %q, fields %+v", detail.Code(), detail.Fields)
	}
}

// Creations and deletions carry the gap of the version that exists; a short copy delay is none.
func TestSnapshotsCarryBackdating(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name, kind string
		gap, want  time.Duration
	}{
		{"backdated creation", changeAdded, backdatedGap, backdatedGap},
		{"fresh creation", changeAdded, 0, 0},
		{"short copy delay", changeAdded, 30 * time.Minute, 0},
		{"backdated deletion", changeDeleted, backdatedGap, backdatedGap},
	} {
		existing := workloadRecord(0)
		existing.Btime, existing.HasBtime = existing.Mtime+int64(fixture.gap), true
		previous, current := integrity.Record{Path: existing.Path}, existing

		if fixture.kind == changeDeleted {
			previous, current = existing, integrity.Record{Path: existing.Path}
		}

		detail, err := newDetail(previous, current, []string{fixture.kind}, "", 0)
		if err != nil || detail.BackdatedBy != fixture.want {
			t.Errorf("%s: backdated by %v, want %v (%v)", fixture.name, detail.BackdatedBy, fixture.want, err)
		}
	}
}
