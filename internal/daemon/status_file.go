package daemon

import (
	"crypto/rand"
	"errors"
	"os"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

func writeStatus(directory, name string, data []byte) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fault.Wrap("open runtime", err)
	}
	defer resource.Close(root)

	temporary := ".status-" + rand.Text()

	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fault.Wrap("create status snapshot", err)
	}
	defer resource.Remove(root, temporary)

	err = writeStatusBytes(file, data)
	if err != nil {
		return err
	}

	err = root.Rename(temporary, name)
	if err != nil {
		return fault.Wrap("replace status snapshot", err)
	}

	// Runtime status is volatile. Rename publishes a complete snapshot to readers;
	// unlike catalog evidence, it needs no durability across a machine restart.
	return nil
}

func writeStatusBytes(file *os.File, data []byte) error {
	_, err := file.Write(data)

	return fault.Wrap("write snapshot", errors.Join(err, file.Close()))
}
