package cli

import (
	"fmt"
	"io"

	"github.com/inode64/fsledger/internal/buildinfo"
	"github.com/inode64/fsledger/internal/fault"
)

// printVersion never loads configuration so broken or missing YAML cannot hide the build identity.
func printVersion(stdout io.Writer) error {
	_, err := fmt.Fprintln(stdout, buildinfo.Line())

	return fault.Wrap("write version", err)
}
