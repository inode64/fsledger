package daemon

import (
	"crypto/rand"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

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

// keepStatusAlive refreshes a snapshot's modification time every statusInterval until stopped: a long
// operation delays the snapshot's content, and status must not report the daemon as stopped meanwhile.
func keepStatusAlive(path string, logger *slog.Logger) func() {
	done := make(chan struct{})

	var refresher sync.WaitGroup

	refresher.Go(func() {
		tick := time.NewTicker(statusInterval)
		defer tick.Stop()

		for {
			select {
			case <-done:
				return
			case now := <-tick.C:
				// Before the first snapshot there is nothing to refresh.
				err := os.Chtimes(path, time.Time{}, now)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					logger.Debug("refresh status snapshot time", "error", err)
				}
			}
		}
	})

	return func() {
		close(done)
		refresher.Wait()
	}
}
