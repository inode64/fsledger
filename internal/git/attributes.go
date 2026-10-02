package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

const literalAttributes = `
# fsledger: retain source bytes; override content conversions from monitored attributes.
* -text -crlf -filter -ident -working-tree-encoding
`

// preserveContent uses Git's highest precedence attributes without changing source files.
func (r *Repository) preserveContent() error {
	metadata, err := os.OpenRoot(filepath.Join(r.Path, ".git"))
	if err != nil {
		return fault.Wrap("open Git metadata", err)
	}
	defer resource.Close(metadata)

	mkdirErr := metadata.MkdirAll("info", 0o700)
	if mkdirErr != nil {
		return fault.Wrap("create Git info directory", mkdirErr)
	}

	previous, err := metadata.ReadFile("info/attributes")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap("read Git attributes", err)
	}

	if strings.HasSuffix(string(previous), literalAttributes) {
		return nil
	}

	writeErr := metadata.WriteFile("info/attributes", append(previous, literalAttributes...), 0o600)

	return fault.Wrap("preserve literal Git content", writeErr)
}
