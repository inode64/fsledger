package catalog

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

// Every new evidence field must participate, including fields outside the policy.
func TestObservationEqualityCoversRecordFields(t *testing.T) {
	t.Parallel()

	original := workloadRecord(0)

	for index := range reflect.TypeFor[integrity.Record]().NumField() {
		name := reflect.TypeFor[integrity.Record]().Field(index).Name
		if name == "Path" || name == "Observed" {
			continue
		}

		changed := original
		field := reflect.ValueOf(&changed).Elem().Field(index)
		//nolint:exhaustive // Unsupported future field kinds must fail this evidence-coverage test.
		switch field.Kind() {
		case reflect.String:
			field.SetString(field.String() + "x")
		case reflect.Int64:
			field.SetInt(field.Int() + 1)
		case reflect.Uint64, reflect.Uint32:
			field.SetUint(field.Uint() + 1)
		case reflect.Bool:
			field.SetBool(!field.Bool())
		case reflect.Slice:
			if name == "Target" {
				changed.Target = []byte("target")
			} else {
				field.Set(reflect.ValueOf([]integrity.Attribute{{Name: []byte("user.test"), Value: []byte("value")}}))
			}
		default:
			t.Fatalf("add mutation for %s", name)
		}

		if sameObservation(original, changed) {
			t.Fatal("ignored evidence field", name)
		}
	}

	original.Observed++
	changed := original
	changed.Observed++
	changed.Target = []byte{}
	changed.ACL = []integrity.Attribute{}

	changed.Xattrs = []integrity.Attribute{}
	if !sameObservation(original, changed) {
		t.Fatal("empty fields or observation time caused rewrite")
	}
}

func BenchmarkUnchangedObservation(b *testing.B) {
	record := workloadRecord(0)
	record.Xattrs = []integrity.Attribute{{Name: []byte("user.payload"), Value: bytes.Repeat([]byte("x"), 4096)}}

	data, err := packRecord(record)
	if err != nil {
		b.Fatal(err)
	}

	previous, err := unpack(data, record.Path)
	if err != nil {
		b.Fatal(err)
	}

	record.Observed++

	b.Run("encode", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			candidate := record
			candidate.Observed = previous.Observed

			encoded, packErr := packRecord(candidate)
			if packErr != nil || !bytes.Equal(encoded, data) {
				b.Fatal(packErr)
			}
		}
	})
	b.Run("compare", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			encoded, packErr := packObservation(record, previous, data)
			if packErr != nil || !bytes.Equal(encoded, data) {
				b.Fatal(packErr)
			}
		}
	})
}
