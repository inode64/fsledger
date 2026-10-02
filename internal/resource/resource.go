// Package resource reports cleanup errors that cannot change an already returned result.
package resource

import (
	"errors"
	"io"
	"log/slog"
	"os"

	"golang.org/x/sys/unix"
)

// Close releases a resource and reports failure.
func Close(closer io.Closer) {
	err := closer.Close()
	if err != nil {
		slog.Warn("resource close failed", "error", err)
	}
}

// FD releases an owned Linux file descriptor without retrying a possibly reused fd.
func FD(fd int) {
	err := unix.Close(fd)
	if err != nil {
		slog.Warn("file descriptor close failed", "error", err)
	}
}

// Remove deletes an abandoned temporary copy; successful rename already removed it.
func Remove(root *os.Root, name string) {
	err := root.Remove(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("temporary copy cleanup failed", "error", err)
	}
}

// OptionalFile closes a possibly absent ancillary kernel descriptor.
func OptionalFile(file *os.File) {
	if file != nil {
		Close(file)
	}
}
