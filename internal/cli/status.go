package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/daemon"
	"github.com/inode64/fsledger/internal/fault"
)

const statusStaleAfter = 5 * time.Second

func printStatus(stdout io.Writer, cfg *config.Config) error {
	var err error

	for _, name := range cfg.Names() {
		//nolint:gosec // The path is configured locally or built from a kernel PID; no remote path input is accepted.
		data, readErr := os.ReadFile(filepath.Join(cfg.Runtime, name+".json"))
		if readErr != nil {
			return fmt.Errorf("status unavailable for %s (daemon may not have run): %w", name, readErr)
		}

		var state daemon.Status

		err = json.Unmarshal(data, &state)
		if err != nil {
			return fmt.Errorf("decode status: %w", err)
		}

		if time.Since(state.Updated) > statusStaleAfter {
			state.Running = false
			state.Warnings = append(state.Warnings, "status snapshot is stale; daemon may be stopped or busy")
		}

		data, err = json.MarshalIndent(state, "", "  ")
		if err != nil {
			return fault.Wrap("encode status", err)
		}

		_, err = fmt.Fprintln(stdout, string(data))
		if err != nil {
			return fault.Wrap("write status", err)
		}
	}

	return nil
}
