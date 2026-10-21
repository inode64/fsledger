package inbound_test

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// accessACL encodes a non-minimal POSIX access ACL so that the kernel stores it as an xattr.
func accessACL(user uint32) []byte {
	const (
		version   = 2
		userObj   = 0x01
		namedUser = 0x02
		groupObj  = 0x04
		mask      = 0x10
		other     = 0x20
		read      = 4
		readWrite = 6
		undefined = ^uint32(0)
	)

	value := binary.LittleEndian.AppendUint32(nil, version)
	for _, entry := range []struct {
		tag, permissions uint16
		id               uint32
	}{
		{tag: userObj, permissions: readWrite, id: undefined},
		{tag: namedUser, permissions: read, id: user},
		{tag: groupObj, permissions: read, id: undefined},
		{tag: mask, permissions: read, id: undefined},
		{tag: other, permissions: read, id: undefined},
	} {
		value = binary.LittleEndian.AppendUint16(value, entry.tag)
		value = binary.LittleEndian.AppendUint16(value, entry.permissions)
		value = binary.LittleEndian.AppendUint32(value, entry.id)
	}

	return value
}

func TestIncomingSymlinkReplacementSkipsUnsupportedAttributes(t *testing.T) {
	t.Parallel()

	for _, attribute := range []string{"user.fsledger-symlink", "system.posix_acl_access"} {
		t.Run(attribute, func(t *testing.T) {
			t.Parallel()
			assertSymlinkReplacement(t, attribute)
		})
	}
}

// assertSymlinkReplacement turns a regular source carrying attribute into an incoming symlink.
func assertSymlinkReplacement(t *testing.T, attribute string) {
	t.Helper()
	state := newFixture(t)
	source := filepath.Join(state.source, "a")
	setSourceAttribute(t, source, attribute)

	err := os.Remove(state.archived("a"))
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink("b", state.archived("a"))
	if err != nil {
		t.Fatal(err)
	}

	plan, err := state.prepare(t)
	if err != nil {
		t.Fatal(err)
	}

	err = plan.Apply(t.Context(), state.repository, func() error { return nil })
	if err != nil || plan.Done != len(plan.Changes) {
		t.Fatal("symlink replacement blocked by an attribute Linux rejects on symlinks", plan.Done, err)
	}

	link, err := os.Readlink(source)
	if err != nil || link != "b" {
		t.Fatal("incoming symlink not applied", link, err)
	}
}

func setSourceAttribute(t *testing.T, source, attribute string) {
	t.Helper()

	var stat unix.Stat_t

	err := unix.Lstat(source, &stat)
	if err != nil {
		t.Fatal(err)
	}

	value := []byte("dropped")
	if attribute == "system.posix_acl_access" {
		value = accessACL(stat.Uid)
	}

	err = unix.Setxattr(source, attribute, value, 0)
	if errors.Is(err, unix.ENOTSUP) {
		t.Skip("temporary filesystem does not support", attribute)
	}

	if err != nil {
		t.Fatal(err)
	}
}
