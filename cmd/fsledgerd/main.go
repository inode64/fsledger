// Command fsledgerd runs the fsledger daemon in the foreground.
package main

import (
	"log/slog"
	"os"

	"github.com/inode64/fsledger/internal/cli"
)

func main() {
	err := cli.Run(append([]string{"run"}, os.Args[1:]...), os.Stdout, os.Stderr)
	if err != nil {
		slog.Error("fsledger failed", "error", err)
		os.Exit(1)
	}
}
