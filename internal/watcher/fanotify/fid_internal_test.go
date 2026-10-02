package fanotify

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFIDRecordValidation(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"file", ".", "../escape", "dir/file", ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data := make([]byte, 24+len(name)+1)
			data[0] = unix.FAN_EVENT_INFO_TYPE_DFID_NAME
			//nolint:gosec // The fixture length is bounded by the literal test names.
			binary.NativeEndian.PutUint16(data[2:4], uint16(len(data)))
			binary.NativeEndian.PutUint32(data[12:16], 4)
			binary.NativeEndian.PutUint32(data[16:20], 1)
			copy(data[24:], name)
			parsed, err := parseFIDs(data)

			valid := name == "file" || name == "."
			if (err == nil) != valid {
				t.Fatalf("name=%q parsed=%+v error=%v", name, parsed, err)
			}

			if valid && parsed[0].name != name {
				t.Fatal("incorrect directory name")
			}

			binary.NativeEndian.PutUint32(data[12:16], 0xffffffff)

			_, err = parseFIDs(data)
			if err == nil {
				t.Fatal("oversized handle accepted")
			}
		})
	}
}

func FuzzFIDRecords(f *testing.F) {
	f.Add([]byte{unix.FAN_EVENT_INFO_TYPE_DFID_NAME, 0, 4, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > eventBufferSize {
			t.Skip()
		}

		_, err := parseFIDs(data)
		if err != nil {
			return
		}
	})
}
