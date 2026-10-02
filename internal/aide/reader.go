// Package aide decodes AIDE database records without executing AIDE configuration.
package aide

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
)

const (
	maxLineBytes       = 4 << 20
	initialBufferBytes = 64 << 10
	gzipMagicBytes     = 2
)

// Reader streams plain or gzip AIDE databases according to their declared columns.
type Reader struct {
	scanner   *bufio.Scanner
	algorithm string
	columns   []string
	hashSize  int
	line      int
	begun     bool
	ended     bool
}

// New detects gzip by magic bytes, independently of the input filename.
func New(input io.Reader, algorithm string) (*Reader, error) {
	buffered := bufio.NewReader(input)

	header, err := buffered.Peek(gzipMagicBytes)
	if err != nil {
		return nil, fault.Wrap("read AIDE header", err)
	}

	var stream io.Reader = buffered
	if header[0] == 0x1f && header[1] == 0x8b {
		stream, err = gzip.NewReader(buffered)
		if err != nil {
			return nil, fault.Wrap("open gzip AIDE database", err)
		}
	}

	// The digest length is a property of the algorithm, so an unsupported one fails before any row is read.
	digest, err := integrity.Hasher(algorithm)
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, initialBufferBytes), maxLineBytes)

	return &Reader{
		scanner: scanner, columns: nil, algorithm: algorithm, hashSize: digest.Size(),
		line: 0, begun: false, ended: false,
	}, nil
}

// Next validates structure through physical EOF, including the gzip checksum.
func (reader *Reader) Next(ctx context.Context) (Entry, error) {
	for reader.scanner.Scan() {
		reader.line++

		err := ctx.Err()
		if err != nil {
			return Entry{}, fault.Wrap("read AIDE database", err)
		}

		line := strings.TrimSpace(reader.scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		entry, row, err := reader.consume(line)
		if err != nil {
			return Entry{}, fmt.Errorf("AIDE line %d: %w", reader.line, err)
		}

		if row {
			return entry, nil
		}
	}

	err := reader.scanner.Err()
	if err != nil {
		return Entry{}, fault.Wrap("read AIDE database", err)
	}

	if !reader.ended {
		return Entry{}, fault.New("incomplete AIDE database: missing @@end_db")
	}

	return Entry{}, io.EOF
}

// IgnoredFields reports columns that this importer does not preserve.
func (reader *Reader) IgnoredFields() []string {
	var ignored []string

	for _, column := range reader.columns {
		if fieldName(column) == "" && column != "attr" && column != reader.algorithm {
			ignored = append(ignored, column)
		}
	}

	return ignored
}

func (reader *Reader) consume(line string) (Entry, bool, error) {
	if reader.ended {
		return Entry{}, false, fault.New("data after @@end_db")
	}

	switch {
	case line == "@@begin_db" && !reader.begun:
		reader.begun = true
	case strings.HasPrefix(line, "@@db_spec ") && reader.begun && reader.columns == nil:
		columns := strings.Fields(line)[1:]

		err := validateColumns(columns)
		if err != nil {
			return Entry{}, false, err
		}

		reader.columns = columns
	case line == "@@end_db" && reader.columns != nil:
		reader.ended = true
	case strings.HasPrefix(line, "@@") || reader.columns == nil:
		return Entry{}, false, fault.New("unexpected AIDE directive or missing @@db_spec")
	default:
		entry, err := decode(reader.columns, strings.Fields(line), reader.algorithm, reader.hashSize)

		return entry, true, err
	}

	return Entry{}, false, nil
}

func validateColumns(columns []string) error {
	seen := make(map[string]bool, len(columns))
	for _, column := range columns {
		if seen[column] {
			return fault.New("duplicate AIDE column: " + column)
		}

		seen[column] = true
	}

	if !seen["name"] || !seen["perm"] {
		return fault.New("AIDE database requires name and perm columns")
	}

	return nil
}
