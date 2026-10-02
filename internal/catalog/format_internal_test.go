package catalog

import (
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

func TestPrintableEvidencePreservesEncoding(t *testing.T) {
	t.Parallel()

	record := integrity.Record{
		Type: "<file>&", Algorithm: fixtureSHA256, Hash: "digest",
		Size: -1, Inode: ^uint64(0), Device: 12, Mode: 0o640, UID: 34, GID: 56, Nlink: 2,
		Mtime: -123, Ctime: 456, Atime: 789, Btime: 1011, HasBtime: true,
		Target: []byte{0xff, '\n'}, AttributesStatus: "available",
		Xattrs: []integrity.Attribute{{Name: []byte{0xff}, Value: []byte{0, '\n'}}},
		ACL:    []integrity.Attribute{},
	}
	want := map[string]string{
		fieldType: `"\u003cfile\u003e\u0026"`, fieldHash: "sha256:digest",
		fieldSize: "-1", fieldInode: "18446744073709551615", fieldDevice: "12", fieldMode: "416",
		fieldUID: "34", fieldGID: "56", fieldNlink: "2", fieldMtime: "-123", fieldCtime: "456", fieldAtime: "789",
		fieldBtime: "1011 (known=true)", fieldHasBtime: "true", fieldTarget: `"/wo="`,
		fieldXattrs: `[{"name":"/w==","value":"AAo="}] (available)`, fieldACL: "[] (available)",
		fieldAttributesStatus: `"available"`,
	}

	for field, expected := range want {
		actual, err := printableField(record, field)
		if err != nil || actual != expected {
			t.Errorf("%s: got %q, want %q: %v", field, actual, expected, err)
		}
	}
}

func TestPrintableEvidencePreservesAbsentValues(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		fieldHash: ":", fieldTarget: "", fieldXattrs: "null ()", fieldACL: "null ()",
		fieldBtime: "0 (known=false)", fieldHasBtime: "false", "unknown": "",
	}
	for field, expected := range want {
		actual, err := printableField(integrity.Record{}, field)
		if err != nil || actual != expected {
			t.Errorf("%s: got %q, want %q: %v", field, actual, expected, err)
		}
	}
}
