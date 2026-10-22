// Command fsledger manages filesystem history.
package main

import (
	"errors"
	"log/slog"
	"os"

	"github.com/inode64/fsledger/internal/cli"
)

// exitChanges is the status of a verification that recorded changes.
const exitChanges = 2

func main() {
	err := cli.Run(os.Args[1:], os.Stdout, os.Stderr)
	if errors.Is(err, cli.ErrChanges) {
		os.Exit(exitChanges)
	}

	if err != nil {
		slog.Error("fsledger failed", "error", err)
		os.Exit(1)
	}
}
