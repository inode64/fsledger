package inbound_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/inbound"
)

const (
	namespaceRun = "FSLEDGER_TEST_NAMESPACE"
	capability   = "security.capability"
	imaHash      = "security.ima"
	evmSignature = "security.evm"
	sha256Bytes  = 32
)

// insideNamespace reruns the calling test as root of private user and mount namespaces whose
// tmpfs accepts security xattrs from an unprivileged user; it reports whether this is that run.
func insideNamespace(t *testing.T) bool {
	t.Helper()

	if os.Getenv(namespaceRun) != "" {
		// AppArmor-confined user namespaces (Ubuntu 24.04 runners) deny the mount with EACCES.
		err := unix.Mount("tmpfs", os.TempDir(), "tmpfs", 0, "")
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skip("user namespace cannot mount tmpfs:", err)
		}

		if err != nil {
			t.Fatal(err)
		}

		return true
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // Rerun this test binary for the calling test only.
	command := exec.CommandContext(t.Context(), executable, "-test.run=^"+t.Name()+"$", "-test.v")
	command.Env = append(os.Environ(), namespaceRun+"=1", "TMPDIR="+t.TempDir())
	command.SysProcAttr = &syscall.SysProcAttr{}
	command.SysProcAttr.Cloneflags = unix.CLONE_NEWUSER | unix.CLONE_NEWNS
	command.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
	command.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}

	output, err := command.CombinedOutput()
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) ||
		errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSPC) {
		t.Skip("unprivileged user namespaces unavailable:", err)
	}

	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}

	if bytes.Contains(output, []byte("--- SKIP")) {
		t.Skipf("%s", output)
	}

	return false
}

// fileCapability encodes VFS_CAP_REVISION_2 data granting an effective CAP_NET_RAW.
func fileCapability() []byte {
	const revisionEffective = 0x02000001

	value := binary.LittleEndian.AppendUint32(nil, revisionEffective)
	for _, word := range []uint32{1 << unix.CAP_NET_RAW, 0, 0, 0} {
		value = binary.LittleEndian.AppendUint32(value, word)
	}

	return value
}

func setSecurityAttribute(t *testing.T, path, attribute string, value []byte) {
	t.Helper()

	err := unix.Lsetxattr(path, attribute, value, 0)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skip("kernel refuses", attribute, err)
	}

	if err != nil {
		t.Fatal(err)
	}
}

func assertAttributeMissing(t *testing.T, path, attribute string) {
	t.Helper()

	_, err := unix.Lgetxattr(path, attribute, nil)
	if !errors.Is(err, unix.ENODATA) {
		t.Fatal("incoming content inherited", attribute, path, err)
	}
}

func TestIncomingRejectsFileCapabilities(t *testing.T) {
	t.Parallel()

	if !insideNamespace(t) {
		return
	}

	state := newFixture(t)
	source := filepath.Join(state.source, "a")
	setSecurityAttribute(t, source, capability, fileCapability())
	write(t, state.archived("a"), "remote")

	_, err := state.prepare(t)
	if !errors.Is(err, inbound.ErrConflict) {
		t.Fatal("binary with file capabilities accepted for replacement", err)
	}

	assertContent(t, source, "initial")
}

func TestIncomingDropsContentBoundAttributes(t *testing.T) {
	t.Parallel()

	if !insideNamespace(t) {
		return
	}

	state := newFixture(t)
	link := filepath.Join(state.source, "link")

	for _, path := range []string{link, state.archived("link")} {
		err := os.Symlink("b", path)
		if err != nil {
			t.Fatal(err)
		}
	}

	var err error

	state.base, err = state.repository.Commit(t.Context(), "Symlink snapshot")
	if err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(state.source, "a")
	setSecurityAttribute(t, source, imaHash, append([]byte{0x04, 0x04}, make([]byte, sha256Bytes)...))
	setSecurityAttribute(t, source, evmSignature, []byte{0x05, 0x02})
	setSecurityAttribute(t, link, capability, fileCapability())
	replaceArchived(t, state)

	plan, err := state.prepare(t)
	if err != nil {
		t.Fatal(err)
	}

	err = plan.Apply(t.Context(), state.repository, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	assertContent(t, source, "remote")
	assertContent(t, link, "remote link")
	assertAttributeMissing(t, source, imaHash)
	assertAttributeMissing(t, source, evmSignature)
	assertAttributeMissing(t, link, capability)
}

// replaceArchived turns a into new content and the link into a regular file.
func replaceArchived(t *testing.T, state fixture) {
	t.Helper()
	write(t, state.archived("a"), "remote")

	err := os.Remove(state.archived("link"))
	if err != nil {
		t.Fatal(err)
	}

	write(t, state.archived("link"), "remote link")
}
