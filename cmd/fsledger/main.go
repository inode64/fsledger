// Command fsledger manages filesystem history.
package main

import (
	"log/slog"
	"os"

	"github.com/inode64/fsledger/internal/cli"
)

func main() {
	err := cli.Run(os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		slog.Error("fsledger failed", "error", err)
		os.Exit(1)
	}
}
