package aide_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/aide"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
)

const (
	sha256Algorithm = "sha256"
	databaseStart   = "@@begin_db\n"
	databaseEnd     = "@@end_db\n"
)

func policy(fields ...string) config.Integrity {
	return config.Integrity{
		Reference: config.ReferenceBaseline,
		Compare:   fields,
		Hash: config.Hash{
			Algorithm: sha256Algorithm, FullScanInterval: time.Hour, OnEvent: true,
			FullScanSchedule: "", FullScanTimezone: "",
		},
	}
}

func parseOne(t *testing.T, database, algorithm string) aide.Entry {
	t.Helper()

	reader, err := aide.New(strings.NewReader(database), algorithm)
	if err != nil {
		t.Fatal(err)
	}

	entry, err := reader.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	_, err = reader.Next(t.Context())
	if !errors.Is(err, io.EOF) {
		t.Fatal("invalid database end", err)
	}

	return entry
}

func TestDifferentColumnOrderAndMetadata(t *testing.T) {
	t.Parallel()

	stamp := base64.StdEncoding.EncodeToString([]byte("1700000000"))
	database := databaseStart + "@@db_spec gid mtime name lcount perm size uid inode atime ctime\n" +
		"42 " + stamp + " /srv/data/a%20b%FF 2 100640 19 7 123 1700000000 1700000001\n" + databaseEnd
	entry := parseOne(t, database, sha256Algorithm)

	err := entry.Validate(policy("type", "mode", "uid", "gid", "size", "inode", "nlink", "mtime", "atime", "ctime"))
	if err != nil {
		t.Fatal(err)
	}

	want := integrity.Record{
		Path: []byte("/srv/data/a b\xff"), Type: "regular", Mode: 0o100640, UID: 7, GID: 42,
		Size: 19, Inode: 123, Nlink: 2, Mtime: 1700000000 * int64(time.Second), Atime: 1700000000 * int64(time.Second),
		Ctime: 1700000001 * int64(time.Second),
	}
	if !bytes.Equal(entry.Record.Path, want.Path) || !entry.Record.TimesInSeconds ||
		len(
			integrity.Differences(
				entry.Record,
				want,
				policy("type", "mode", "uid", "gid", "size", "inode", "nlink", "mtime", "atime", "ctime").Compare,
			),
		) != 0 {
		t.Fatalf("metadata changed: %+v", entry.Record)
	}
}

func TestSupportedHashes(t *testing.T) {
	t.Parallel()

	for _, algorithm := range []string{sha256Algorithm, "sha512", "sha512_256", "sha3_256", "sha3_512"} {
		t.Run(algorithm, func(t *testing.T) {
			t.Parallel()

			digest, err := integrity.Hasher(algorithm)
			if err != nil {
				t.Fatal(err)
			}

			value := base64.StdEncoding.EncodeToString(digest.Sum(nil))
			entry := parseOne(
				t,
				databaseStart+"@@db_spec name perm "+algorithm+"\n/etc/setting 100600 "+value+"\n"+databaseEnd,
				algorithm,
			)
			selected := policy("hash")

			selected.Hash.Algorithm = algorithm

			err = entry.Validate(selected)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMasksDoNotTurnAbsentFieldsIntoZero(t *testing.T) {
	t.Parallel()

	entry := parseOne(
		t,
		databaseStart+"@@db_spec name perm uid attr\n/etc/setting 100600 0 1\n"+databaseEnd,
		sha256Algorithm,
	)

	err := entry.Validate(policy("uid"))
	if err == nil {
		t.Fatal("unobserved UID became root")
	}
}

func TestSymlinkEscapesAndIgnoredFields(t *testing.T) {
	t.Parallel()

	database := databaseStart + "@@db_spec name perm lname acl\n/etc/link 120777 00%20target POSIX,0,0\n" + databaseEnd

	entry := parseOne(t, database, sha256Algorithm)
	if string(entry.Record.Target) != "0 target" {
		t.Fatalf("changed target: %q", entry.Record.Target)
	}

	err := entry.Validate(policy("type", "target", "hash"))
	if err != nil {
		t.Fatal(err)
	}

	err = entry.Validate(policy("acl"))
	if err == nil {
		t.Fatal("unsupported ACL approved")
	}

	reader, err := aide.New(strings.NewReader(database), sha256Algorithm)
	if err != nil {
		t.Fatal(err)
	}

	_, err = reader.Next(t.Context())
	if err != nil || !slices.Equal(reader.IgnoredFields(), []string{"acl"}) {
		t.Fatal("unsupported column not reported", err)
	}
}

func TestRejectMalformedDatabase(t *testing.T) {
	t.Parallel()

	validRow := "/etc/a 100600\n"

	header := databaseStart + "@@db_spec name perm\n"
	for _, database := range []string{
		header + validRow,
		header + validRow + databaseEnd + validRow,
		header + "/etc/a 100600 extra\n" + databaseEnd,
		header + "/etc/%GG 100600\n" + databaseEnd,
		header + "/etc/%00 100600\n" + databaseEnd,
		header + "/etc/../a 100600\n" + databaseEnd,
		header + "relative 100600\n" + databaseEnd,
		header + "/etc/a 777\n" + databaseEnd,
		databaseStart + "@@db_spec name perm name\n" + databaseEnd,
		databaseStart + "@@db_spec name uid\n" + databaseEnd,
	} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()

			reader, err := aide.New(strings.NewReader(database), sha256Algorithm)
			if err != nil {
				t.Fatal(err)
			}

			for err == nil {
				_, err = reader.Next(t.Context())
			}

			if errors.Is(err, io.EOF) {
				t.Fatal("malformed database accepted")
			}
		})
	}
}

func TestGzipChecksumIsVerifiedAfterEndMarker(t *testing.T) {
	t.Parallel()

	var compressed bytes.Buffer

	writer := gzip.NewWriter(&compressed)

	_, err := io.WriteString(writer, databaseStart+"@@db_spec name perm\n/etc/a 100600\n"+databaseEnd)
	if err != nil {
		t.Fatal(err)
	}

	err = writer.Close()
	if err != nil {
		t.Fatal(err)
	}

	data := compressed.Bytes()
	data[len(data)-1] ^= 1

	reader, err := aide.New(bytes.NewReader(data), sha256Algorithm)
	if err != nil {
		t.Fatal(err)
	}

	for err == nil {
		_, err = reader.Next(t.Context())
	}

	if !errors.Is(err, gzip.ErrChecksum) {
		t.Fatal("corrupt gzip accepted", err)
	}
}

func TestRejectTransformedHashes(t *testing.T) {
	t.Parallel()

	const sha256Bit = uint64(1 << 30)
	for _, transform := range []uint64{1 << 40, 1 << 41} {
		database := fmt.Sprintf(
			"%s@@db_spec name perm sha256 attr\n/etc/a 100600 %s %d\n%s",
			databaseStart,
			base64.StdEncoding.EncodeToString(make([]byte, sha256.Size)),
			sha256Bit|transform,
			databaseEnd,
		)
		entry := parseOne(t, database, sha256Algorithm)

		err := entry.Validate(policy("hash"))
		if err == nil || !strings.Contains(err.Error(), "growing/compressed") {
			t.Fatal("transformed hash accepted", err)
		}
	}
}
