// Package logging opens the daemon's configured log destination.
package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// Logger owns its optional file for the lifetime of the daemon.
type Logger struct {
	*slog.Logger

	file *os.File
}

// Open uses stderr by default; debug overrides the configured severity.
func Open(options config.Logging, stderr io.Writer, debug bool) (*Logger, error) {
	level, err := options.Level.SlogLevel()
	if err != nil {
		return nil, err
	}

	if debug {
		level = slog.LevelDebug
	}

	var file *os.File

	if options.File != "" {
		file, err = openFile(options.File)
		if err != nil {
			return nil, err
		}

		stderr = file
	}

	return &Logger{Logger: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})), file: file}, nil
}

// Close releases only the owned file, leaving the caller's stderr open.
func (logger *Logger) Close() error {
	if logger.file == nil {
		return nil
	}

	return fault.Wrap("close log file", logger.file.Close())
}

func openFile(path string) (*os.File, error) {
	logPathErr := config.CheckLogPath(path)
	if logPathErr != nil {
		return nil, logPathErr
	}

	mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700)
	if mkdirErr != nil {
		return nil, fault.Wrap("create log directory", mkdirErr)
	}

	fd, err := unix.Open(
		path,
		unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0o600,
	)
	if err != nil {
		return nil, fault.Wrap("open log file", err)
	}

	file := os.NewFile(uintptr(fd), path)

	info, err := file.Stat()
	if err != nil {
		resource.Close(file)

		return nil, fault.Wrap("stat log file", err)
	}

	if !info.Mode().IsRegular() {
		resource.Close(file)

		return nil, fault.New("log file must be a regular file")
	}

	return file, nil
}
