package fanotify

import (
	"encoding/binary"
	"math"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/watcher"
)

func TestKernelOverflowRecord(t *testing.T) {
	t.Parallel()

	buffer := make([]byte, metadataSize)
	binary.NativeEndian.PutUint32(buffer, uint32(metadataSize))
	buffer[4] = unix.FANOTIFY_METADATA_VERSION
	binary.NativeEndian.PutUint16(buffer[6:8], uint16(metadataSize))
	binary.NativeEndian.PutUint64(buffer[8:16], unix.FAN_Q_OVERFLOW)
	binary.NativeEndian.PutUint32(buffer[16:20], math.MaxUint32)

	detector := new(Watcher)

	detector.queue = watcher.NewQueue(1)

	err := detector.decode(buffer)
	if err != nil {
		t.Fatal(err)
	}

	if !detector.Dirty() {
		t.Fatal("kernel overflow did not mark dirty")
	}
}

func TestTruncatedRecord(t *testing.T) {
	t.Parallel()

	detector := new(Watcher)

	err := detector.decode([]byte{1, 2, 3})
	if err == nil {
		t.Fatal("truncated metadata accepted")
	}

	readPIDFDDescriptor, readPIDFDErr := readPIDFD([]byte{4, 0, 0})
	if readPIDFDErr == nil || readPIDFDDescriptor != nil {
		t.Fatal("truncated info accepted")
	}
}
