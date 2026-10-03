package integrity

import (
	"errors"
	"io"

	"github.com/inode64/fsledger/internal/fault"
)

// CheckContentSize rejects stable virtual files whose declared size does not
// describe their readable bytes. Callers must also verify source stability.
func CheckContentSize(source io.Reader, expected, copied int64) error {
	if copied != expected {
		return fault.New("source size differs from readable content")
	}

	var extra [1]byte

	count, err := source.Read(extra[:])
	if count != 0 {
		return fault.New("source size differs from readable content")
	}

	if err != nil && !errors.Is(err, io.EOF) {
		return fault.Wrap("check source end", err)
	}

	return nil
}
