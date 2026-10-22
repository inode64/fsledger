package git

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// literalAttributes also unsets the export rules git archive honors, so a restore neither
// omits export-ignore paths nor expands export-subst placeholders.
const literalAttributes = `
# fsledger: retain source bytes and paths; override monitored conversion and export attributes.
* -text -crlf -filter -ident -working-tree-encoding -export-ignore -export-subst
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

	current, err := metadata.ReadFile("info/attributes")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap("read Git attributes", err)
	}

	// fsledger owns the mirror's info/attributes: any other content is replaced.
	if string(current) == literalAttributes {
		return nil
	}

	writeErr := metadata.WriteFile("info/attributes", []byte(literalAttributes), 0o600)

	return fault.Wrap("preserve literal Git content", writeErr)
}
