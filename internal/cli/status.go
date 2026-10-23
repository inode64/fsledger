package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/daemon"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	statusStaleAfter = 5 * time.Second
	statusMissing    = "no status snapshot: daemon not running or repository not started yet"
)

// missingStatus stands in for an absent snapshot so the output keeps one JSON object per repository.
type missingStatus struct {
	Repository string   `json:"repository"`
	Type       string   `json:"type"`
	Warnings   []string `json:"warnings"`
	Running    bool     `json:"running"`
}

func printStatus(stdout io.Writer, cfg *config.Config) error {
	var err error

	for _, name := range cfg.Names() {
		data, refreshed, readErr := readStatus(filepath.Join(cfg.Runtime, name+".json"))
		if errors.Is(readErr, fs.ErrNotExist) {
			// Snapshots appear once a repository starts and do not survive a reboot. Like a stale one,
			// an absent snapshot is a reported state, not a failure that hides the other repositories.
			err = writeStatus(stdout, missingStatus{
				Repository: name, Type: cfg.Repositories[name].Type, Warnings: []string{statusMissing}, Running: false,
			})
			if err != nil {
				return err
			}

			continue
		}

		if readErr != nil {
			return fmt.Errorf("status unavailable for %s: %w", name, readErr)
		}

		var state daemon.Status

		err = json.Unmarshal(data, &state)
		if err != nil {
			return fmt.Errorf("decode status: %w", err)
		}

		// The daemon refreshes the file's time while a long operation delays its content.
		switch {
		case time.Since(refreshed) > statusStaleAfter:
			state.Running = false
			state.Warnings = append(state.Warnings, "status snapshot is stale; daemon may be stopped")
		case time.Since(state.Updated) > statusStaleAfter:
			state.Warnings = append(state.Warnings, "repository busy; status content dates from "+
				state.Updated.Format(time.RFC3339))
		}

		err = writeStatus(stdout, state)
		if err != nil {
			return err
		}
	}

	return nil
}

// readStatus returns a snapshot with its modification time, which the daemon keeps fresh while it runs.
func readStatus(path string) ([]byte, time.Time, error) {
	//nolint:gosec // The path is the configured runtime directory and a configured repository name.
	file, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, fault.Wrap("open status", err)
	}
	defer resource.Close(file)

	info, err := file.Stat()
	if err != nil {
		return nil, time.Time{}, fault.Wrap("inspect status", err)
	}

	data, err := io.ReadAll(file)

	return data, info.ModTime(), fault.Wrap("read status", err)
}

func writeStatus(stdout io.Writer, state any) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fault.Wrap("encode status", err)
	}

	_, err = fmt.Fprintln(stdout, string(data))

	return fault.Wrap("write status", err)
}
